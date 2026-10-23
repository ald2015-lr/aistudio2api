package setup

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

type setupStrings []string

func (values *setupStrings) String() string {
	return strings.Join(*values, ",")
}

func (values *setupStrings) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("参数值不能为空")
	}
	*values = append(*values, value)
	return nil
}

type setupOptions struct {
	storageState string
	login        bool
	chromeRoot   string
	profiles     setupStrings
	emails       setupStrings
	// proxy 为登录、导入与验证实际使用的出口：--proxy 的值，没传时为 .env 的 PROXY（IP 池开启时 Run 改用 IP 池出口）
	proxy string
	// accountProxy 为写进 account.json 的账户固定代理：只在显式传了 --proxy 时取值；没传时为空，账户跟随默认代理与 IP 池
	// （原先把 .env 的 PROXY 写进账户，开启 IP 池后这些账户会悄悄绕过它）
	accountProxy string
	proxySet     bool
	locale       string
	localeSet    bool
	timezone     string
}

// Run 执行本机 Chrome 导入、文件导入或隔离登录
func Run(ctx context.Context, cfg config.Config, args []string) error {
	// 本机代理中转（setup --login 带账号密码的代理）的日志写到标准错误，遇到 407 等能看到原因；日志里不含账号密码
	proxyrelay.SetLogger(func(level, message string) { fmt.Fprintf(os.Stderr, "%s %s\n", level, message) })
	options, err := parseSetupFlags(args, cfg)
	if err != nil {
		return err
	}
	if err := validateSetupRoot(cfg.AuthStates); err != nil {
		return err
	}
	store := aistudio.NewAccountStore(cfg.AuthStates)
	if options.storageState != "" {
		return importStorageState(store, options)
	}
	if !options.proxySet && cfg.ProxyPool.Enabled() {
		// 没传 --proxy 且 IP 池开启：登录与导入也用 IP 池出口（静态 IP 经端口锁与主进程互斥），PROXY 为空时不会从服务器真实 IP 登录
		proxy, release, notice := setupPoolProxy(cfg.ProxyPool)
		defer release()
		if proxy != "" {
			options.proxy = proxy
		}
		if notice != "" {
			fmt.Fprintln(os.Stderr, notice)
		}
	}
	if options.login {
		driver, err := defaultSetupLoginDriver(ctx, cfg)
		if err != nil {
			return err
		}
		return importIsolatedLogin(ctx, store, options, driver)
	}
	return importChromeAccounts(ctx, cfg, store, options, os.Stdin, os.Stdout)
}

func importStorageState(store *aistudio.AccountStore, options setupOptions) error {
	state, err := aistudio.LoadStorageState(options.storageState)
	if err != nil {
		return err
	}
	if _, err := aistudio.NewSigner().Sign(state); err != nil {
		return fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
	}
	label := defaultSetupLabel(options.storageState)
	if extension, exists, err := state.AuthExtension(); err != nil {
		return err
	} else if exists && strings.TrimSpace(extension.Source.Email) != "" {
		label = extension.Source.Email
	}
	account, publishLease, err := store.Create(setupAccountConfig(label, options), state)
	if err != nil {
		return err
	}
	if err := publishLease.Release(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "账户已保存: %s\n", account.Config.Label)
	return nil
}

func importIsolatedLogin(
	ctx context.Context,
	store *aistudio.AccountStore,
	options setupOptions,
	driver aistudio.IsolatedLoginDriver,
) (resultErr error) {
	loginDirectory, err := os.MkdirTemp("", "aistudio2api-login-*")
	if err != nil {
		return fmt.Errorf("创建隔离登录目录: %w", err)
	}
	defer os.RemoveAll(loginDirectory)
	result, err := driver.Login(ctx, aistudio.IsolatedLoginRequest{
		AccountID: "setup", Directory: loginDirectory, Proxy: options.proxy,
		Locale: options.locale, Timezone: options.timezone,
	})
	if err != nil {
		return err
	}
	if _, err := aistudio.NewSigner().Sign(result.StorageState); err != nil {
		return fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
	}
	account, publishLease, err := store.Create(setupAccountConfig(result.Email, options), result.StorageState)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, publishLease.Release())
	}()
	if err := camoufoxnative.PersistAccountFingerprint(loginDirectory, account.Directory); err != nil {
		return errors.Join(err, store.Delete(account))
	}
	fmt.Fprintf(os.Stdout, "账户已保存: %s\n", account.Config.Label)
	if result.DriveError != "" {
		fmt.Fprintf(os.Stdout, "Drive 授权失败: %s\n", result.DriveError)
	}
	return nil
}

func importChromeAccounts(ctx context.Context, cfg config.Config, store *aistudio.AccountStore, options setupOptions, input io.Reader, output io.Writer) error {
	root := options.chromeRoot
	if root == "" {
		var err error
		root, err = chromeauth.DefaultChromeRoot()
		if err != nil {
			return err
		}
	}
	var accountIDs []string
	if len(options.profiles) == 0 && len(options.emails) == 0 {
		accounts, err := chromeauth.Discover(root)
		if err != nil {
			return err
		}
		accountIDs, err = promptChromeAccounts(accounts, input, output)
		if err != nil {
			return err
		}
	}
	results, err := chromeauth.Import(ctx, chromeauth.ImportOptions{
		ChromeRoot: root, Proxy: options.proxy, Profiles: options.profiles, Emails: options.emails, AccountIDs: accountIDs,
	})
	if err != nil {
		return err
	}
	modelCounts := make([]int, len(results))
	for index := range results {
		verifyContext, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		verification, verifyErr := chromeauth.Verify(verifyContext, &results[index].State, options.proxy)
		cancel()
		if verifyErr != nil {
			return fmt.Errorf("验证 %s: %w", results[index].Email, verifyErr)
		}
		modelCounts[index] = verification.ModelCount
	}
	for index, result := range results {
		accountOptions := options
		if !options.localeSet && result.Locale != "" {
			accountOptions.locale = result.Locale
		}
		_, publishLease, err := store.Create(setupAccountConfig(result.Email, accountOptions), result.State)
		if err != nil {
			return err
		}
		if err := publishLease.Release(); err != nil {
			return err
		}
		fmt.Fprintf(output, "已导入: %s (%s)，%d 个模型\n", result.Email, result.Profile, modelCounts[index])
	}
	return nil
}

func promptChromeAccounts(accounts []chromeauth.Account, input io.Reader, output io.Writer) ([]string, error) {
	available := make([]chromeauth.Account, 0, len(accounts))
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "编号\t状态\tProfile\t显示名\t邮箱")
	for _, account := range accounts {
		index := "-"
		status := "不可导入"
		if account.Importable {
			available = append(available, account)
			index = strconv.Itoa(len(available))
			status = "可导入"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", index, status, account.Profile, account.DisplayName, account.Email)
	}
	if err := table.Flush(); err != nil {
		return nil, fmt.Errorf("输出 Chrome 账号列表: %w", err)
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("本机 Chrome 没有可导入账号")
	}
	fmt.Fprint(output, "请输入逗号分隔的编号: ")
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("读取账号选择: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, fmt.Errorf("未选择账号")
	}
	selected := make([]string, 0)
	seen := make(map[int]struct{})
	for _, raw := range strings.Split(line, ",") {
		index, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || index < 1 || index > len(available) {
			return nil, fmt.Errorf("账号编号 %q 无效", strings.TrimSpace(raw))
		}
		if _, exists := seen[index]; exists {
			continue
		}
		seen[index] = struct{}{}
		selected = append(selected, available[index-1].ID)
	}
	return selected, nil
}

func parseSetupFlags(args []string, cfg config.Config) (setupOptions, error) {
	flags := flag.NewFlagSet("aistudio2api setup", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Chrome 导入: aistudio2api setup")
		fmt.Fprintln(flags.Output(), "文件导入: aistudio2api setup --storage-state <file>")
		fmt.Fprintln(flags.Output(), "隔离登录: aistudio2api setup --login")
		flags.PrintDefaults()
	}
	var profiles setupStrings
	var emails setupStrings
	flags.Var(&profiles, "profile", "要导入的 Chrome Profile，可重复")
	flags.Var(&emails, "email", "要导入的 Google 邮箱，可重复")
	storageState := flags.String("storage-state", "", "Playwright storage state 文件")
	login := flags.Bool("login", false, "使用隔离 Camoufox 登录")
	chromeRoot := flags.String("chrome-root", "", "Chrome User Data 目录")
	// 默认值留空：.env 的 PROXY 可能带账号密码，-h 与参数解析出错时 PrintDefaults 会把默认值打印出来
	proxy := flags.String("proxy", "", "账户固定代理，可带账号密码，写法同 .env 的 PROXY；不指定时使用 .env 的值")
	locale := flags.String("locale", aistudio.DefaultAccountLocale(), "账户语言")
	timezone := flags.String("timezone", aistudio.DefaultAccountTimezone(), "账户时区")
	if err := flags.Parse(args); err != nil {
		return setupOptions{}, err
	}
	if flags.NArg() != 0 {
		proxyGiven := false
		flags.Visit(func(value *flag.Flag) { proxyGiven = proxyGiven || value.Name == "proxy" })
		if proxyGiven {
			// --proxy 的值里有空格又没加引号时，后半段（可能是密码的一部分）成了位置参数，整体不回显
			return setupOptions{}, errors.New("未知参数（已隐藏：可能是 --proxy 值的一部分，值里有空格时请整体加引号）")
		}
		// 位置参数可能是忘了写 --proxy 的带账号密码代理，错误会写进日志，不回显账号密码
		return setupOptions{}, fmt.Errorf("未知参数 %q", config.RedactArgument(flags.Arg(0)))
	}
	// 没传 --proxy 时使用 .env 的值（Load 已规范化并校验）
	options := setupOptions{
		storageState: strings.TrimSpace(*storageState), login: *login,
		chromeRoot: strings.TrimSpace(*chromeRoot), profiles: profiles, emails: emails,
		proxy:  strings.TrimSpace(cfg.Proxy),
		locale: strings.TrimSpace(*locale), timezone: strings.TrimSpace(*timezone),
	}
	flags.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "locale":
			options.localeSet = true
		case "proxy":
			options.proxySet = true
		}
	})
	chromeSelection := options.chromeRoot != "" || len(options.profiles) != 0 || len(options.emails) != 0
	if options.storageState != "" && (options.login || chromeSelection) {
		return setupOptions{}, fmt.Errorf("--storage-state 不能与 Chrome 导入或 --login 同时使用")
	}
	if options.login && chromeSelection {
		return setupOptions{}, fmt.Errorf("--login 不能与 Chrome 导入参数同时使用")
	}
	if options.locale == "" || options.timezone == "" {
		return setupOptions{}, fmt.Errorf("locale 和 timezone 不能为空")
	}
	if options.proxySet {
		// 显式传了 --proxy：规范化成上游 URL（--proxy "" 表示直连），错误文案不含输入内容；同时写进账户（账户固定代理）
		normalized, err := config.NormalizeProxy(*proxy)
		if err != nil {
			return setupOptions{}, err
		}
		options.proxy = normalized
		options.accountProxy = normalized
	}
	return options, nil
}

func setupAccountConfig(label string, options setupOptions) aistudio.AccountConfig {
	accountConfig := aistudio.DefaultAccountConfig(label)
	accountConfig.Proxy = options.accountProxy
	accountConfig.Locale = options.locale
	accountConfig.Timezone = options.timezone
	return accountConfig
}

func defaultSetupLabel(storageState string) string {
	parent := filepath.Base(filepath.Clean(filepath.Dir(storageState)))
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return "Default"
	}
	return parent
}

func defaultSetupLoginDriver(ctx context.Context, cfg config.Config) (aistudio.IsolatedLoginDriver, error) {
	camoufoxPath, err := camoufoxnative.FindExecutable(ctx)
	if err != nil {
		return nil, err
	}
	return aistudio.NewNativeLoginDriver(camoufoxPath, cfg.RequestTimeout)
}

func validateSetupRoot(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return fmt.Errorf("AISTUDIO_AUTH_STATES 不能为空")
	}
	if strings.Contains(root, ",") {
		return fmt.Errorf("setup 需要 AISTUDIO_AUTH_STATES 指向单个账户目录")
	}
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取账户目录: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("setup 需要 AISTUDIO_AUTH_STATES 指向账户目录")
	}
	return nil
}
