package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// requestBodyLimit 是单个请求或响应正文保存的最大字节数
const requestBodyLimit = 64 << 10

// usageMaxSpan 是一次用量查询允许的最长时间范围
const usageMaxSpan = 93 * 24 * time.Hour

// usageMaxBuckets 是一次用量查询允许的最多分桶数
const usageMaxBuckets = 1500

// ErrRequestBodyNotFound 表示请求没有保存正文
var ErrRequestBodyNotFound = errors.New("请求正文不存在")

// RequestLedger 提供请求用量统计、请求记录与可选的正文记录
type RequestLedger interface {
	BodyCapture() bool
	SaveBody(RequestBody)
	Usage(context.Context, UsageQuery) (UsageReport, error)
	Records(context.Context, UsageRecordQuery) (UsageRecordPage, error)
	ExportRecords(context.Context, UsageRecordQuery, io.Writer) error
	RequestBody(context.Context, string) (RequestBody, error)
}

// UsageDimensions 是用量可筛选与分组的维度
var UsageDimensions = []string{"model", "account", "channel", "protocol", "state"}

// UsageFilters 按维度限定参与统计的请求，同一维度内多个取值为或关系
type UsageFilters map[string][]string

// UsageQuery 选择用量统计的时间范围、分桶、时区、筛选与堆叠维度
type UsageQuery struct {
	From     time.Time
	To       time.Time
	Bucket   time.Duration
	Location *time.Location
	Filters  UsageFilters
	Stack    string
}

// UsageLatency 汇总一组请求的平均值与分位耗时
type UsageLatency struct {
	AvgMS float64 `json:"avg_ms"`
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
}

// UsageStats 汇总一组请求的结果、token 与耗时；成功包含工具调用、输出上限与上游终止
type UsageStats struct {
	Requests        int64        `json:"requests"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	Canceled        int64        `json:"canceled"`
	RateLimited     int64        `json:"rate_limited"`
	InputTokens     int64        `json:"input_tokens"`
	ReasoningTokens int64        `json:"reasoning_tokens"`
	ReplyTokens     int64        `json:"reply_tokens"`
	TotalTokens     int64        `json:"total_tokens"`
	Duration        UsageLatency `json:"duration"`
	FirstEvent      UsageLatency `json:"first_event"`
	QueueAvgMS      float64      `json:"queue_avg_ms"`
	// DowngradeChecked 为经过降级判定的请求数，DowngradeRejected 为其中因降级被拒绝的请求数
	DowngradeChecked  int64 `json:"downgrade_checked"`
	DowngradeRejected int64 `json:"downgrade_rejected"`
	// Replies 为参与重复回复统计的请求数（回复足够长），DuplicateReplies 为其中与近期某次回复完全相同的请求数
	Replies          int64      `json:"replies"`
	DuplicateReplies int64      `json:"duplicate_replies"`
	LastAt           *time.Time `json:"last_at,omitempty"`
}

// UsageBucket 保存一个时间分桶的汇总
type UsageBucket struct {
	At time.Time `json:"at"`
	UsageStats
}

// UsageGroup 保存一个维度取值的汇总
type UsageGroup struct {
	Key string `json:"key"`
	UsageStats
}

// UsagePair 保存账户与模型组合的汇总
type UsagePair struct {
	Account string `json:"account"`
	Model   string `json:"model"`
	UsageStats
}

// UsageSeries 是堆叠趋势中一个维度取值逐桶的请求数与 token，Other 合并排名之外的取值
type UsageSeries struct {
	Key      string  `json:"key"`
	Other    bool    `json:"other,omitempty"`
	Requests []int64 `json:"requests"`
	Tokens   []int64 `json:"tokens"`
}

// UsageRecent 汇总最近几分钟的请求与 token，用于计算当前吞吐
type UsageRecent struct {
	Minutes  int   `json:"minutes"`
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

// UsageReport 返回用量看板的汇总、上一周期、逐桶趋势、堆叠序列、分项与筛选候选
type UsageReport struct {
	From          time.Time               `json:"from"`
	To            time.Time               `json:"to"`
	BucketSeconds int64                   `json:"bucket_seconds"`
	GeneratedAt   time.Time               `json:"generated_at"`
	LatestAt      *time.Time              `json:"latest_at,omitempty"`
	Totals        UsageStats              `json:"totals"`
	Previous      UsageStats              `json:"previous"`
	Recent        UsageRecent             `json:"recent"`
	Buckets       []UsageBucket           `json:"buckets"`
	StackBy       string                  `json:"stack_by"`
	Series        []UsageSeries           `json:"series"`
	Groups        map[string][]UsageGroup `json:"groups"`
	Pairs         []UsagePair             `json:"pairs"`
	Options       map[string][]string     `json:"options"`
}

// RequestAttempt 记录一次未成功的上游尝试
type RequestAttempt struct {
	Account    string `json:"account"`
	Channel    string `json:"channel"`
	Error      string `json:"error"`
	DurationMS int64  `json:"duration_ms"`
}

// UsageRecordQuery 选择请求记录的时间范围、筛选、状态码、关键字与分页位置
type UsageRecordQuery struct {
	From    time.Time
	To      time.Time
	Filters UsageFilters
	Status  int
	Search  string
	Before  *UsageCursor
	Limit   int
}

// UsageCursor 是请求记录分页位置，下一页从该记录之后的更早记录开始
type UsageCursor struct {
	Time time.Time
	ID   string
}

// EncodeUsageCursor 将分页位置编码为 URL 安全的字符串
func EncodeUsageCursor(cursor UsageCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(cursor.Time.UnixMilli(), 10) + "|" + cursor.ID))
}

// decodeUsageCursor 解析 EncodeUsageCursor 生成的分页位置
func decodeUsageCursor(value string) (*UsageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("cursor 无效")
	}
	at, id, found := strings.Cut(string(raw), "|")
	milliseconds, err := strconv.ParseInt(at, 10, 64)
	if !found || err != nil || id == "" {
		return nil, fmt.Errorf("cursor 无效")
	}
	return &UsageCursor{Time: time.UnixMilli(milliseconds).UTC(), ID: id}, nil
}

// UsageRecord 是一条已完成请求的账本记录
type UsageRecord struct {
	ID              string           `json:"id"`
	Time            time.Time        `json:"time"`
	Protocol        string           `json:"protocol"`
	Path            string           `json:"path"`
	Model           string           `json:"model"`
	Account         string           `json:"account"`
	Channel         string           `json:"channel"`
	Status          int              `json:"status"`
	State           string           `json:"state"`
	DurationMS      int64            `json:"duration_ms"`
	FirstEventMS    int64            `json:"first_event_ms"`
	QueueMS         int64            `json:"queue_ms"`
	InputTokens     int64            `json:"input_tokens"`
	ReasoningTokens int64            `json:"reasoning_tokens"`
	ReplyTokens     int64            `json:"reply_tokens"`
	TotalTokens     int64            `json:"total_tokens"`
	ToolCalls       int              `json:"tool_calls"`
	Error           string           `json:"error"`
	Attempts        []RequestAttempt `json:"attempts"`
	// ServedModel 为上游标明的实际服务模型（只在与请求的模型系列不同时有值）
	ServedModel string `json:"served_model,omitempty"`
	// Downgrade 为降级判定结论：rejected、passed、unjudged；没有经过判定时为空
	Downgrade string `json:"downgrade,omitempty"`
	// ReplyHash 为回复正文指纹，Duplicate 表示近期出现过完全相同的回复
	ReplyHash string `json:"reply_hash,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
	HasBody   bool   `json:"has_body"`
}

// UsageRecordPage 是按完成时间倒序的一页请求记录，NextCursor 为空表示没有更早的记录
type UsageRecordPage struct {
	Items      []UsageRecord `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// RequestBody 保存一次请求截断后的请求与响应正文
type RequestBody struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Request      string    `json:"request"`
	RequestSize  int64     `json:"request_size"`
	Response     string    `json:"response"`
	ResponseSize int64     `json:"response_size"`
}

// CountTokensPath 判断路径是否为 token 计数接口（Anthropic /v1/messages/count_tokens 与 Gemini :countTokens）。
// 计数请求不生成内容，也不计入用量账本，否则会抬高请求数并拉低平均耗时
func CountTokensPath(path string) bool {
	return strings.HasSuffix(path, "/count_tokens") || strings.HasSuffix(path, ":countTokens")
}

// registerLedger 注册用量统计、请求记录与请求正文接口（与其他控制面接口一样需要管理令牌）
func (s *server) registerLedger(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/usage", s.handleUsage)
	mux.HandleFunc("GET /api/usage/records", s.handleUsageRecords)
	mux.HandleFunc("GET /api/usage/records.csv", s.handleUsageExport)
	mux.HandleFunc("GET /api/requests/{id}/body", s.handleRequestBody)
}

// usageBucketSizes 是允许手动选择的分桶时长
var usageBucketSizes = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 24 * time.Hour}

// autoUsageBucket 按时间范围长度选择分桶时长
func autoUsageBucket(span time.Duration) time.Duration {
	switch {
	case span <= 2*time.Hour:
		return time.Minute
	case span <= 12*time.Hour:
		return 5 * time.Minute
	case span <= 8*24*time.Hour:
		return time.Hour
	}
	return 24 * time.Hour
}

// parseUsageRange 读取 from 与 to，校验先后顺序与最长范围
func parseUsageRange(values map[string][]string) (time.Time, time.Time, error) {
	get := func(name string) string {
		if list := values[name]; len(list) > 0 {
			return list[0]
		}
		return ""
	}
	from, err := time.Parse(time.RFC3339, get("from"))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("from 必须是 RFC 3339 时间")
	}
	to, err := time.Parse(time.RFC3339, get("to"))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("to 必须是 RFC 3339 时间")
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from 必须早于 to")
	}
	if to.Sub(from) > usageMaxSpan {
		return time.Time{}, time.Time{}, fmt.Errorf("时间范围不能超过 93 天")
	}
	return from.UTC(), to.UTC(), nil
}

// parseUsageFilters 读取各维度的筛选值，逗号分隔多个取值，空值与重复值被忽略
func parseUsageFilters(values map[string][]string) UsageFilters {
	filters := UsageFilters{}
	for _, dimension := range UsageDimensions {
		for _, value := range values[dimension] {
			for _, item := range strings.Split(value, ",") {
				if item = strings.TrimSpace(item); item != "" && !slices.Contains(filters[dimension], item) {
					filters[dimension] = append(filters[dimension], item)
				}
			}
		}
	}
	return filters
}

// ledgerErrorStatus 返回账本查询失败的状态码：客户端取消与超时按原语义，其余是本地数据库错误
func ledgerErrorStatus(err error) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return statusFromError(err)
	}
	return http.StatusInternalServerError
}

func (s *server) handleUsage(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	from, to, err := parseUsageRange(values)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	query := UsageQuery{From: from, To: to, Location: time.UTC, Filters: parseUsageFilters(values), Stack: "model"}
	if name := values.Get("tz"); name != "" {
		if query.Location, err = time.LoadLocation(name); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "tz 必须是 IANA 时区名")
			return
		}
	}
	query.Bucket = autoUsageBucket(to.Sub(from))
	if value := values.Get("bucket"); value != "" && value != "auto" {
		seconds, err := strconv.Atoi(value)
		query.Bucket = time.Duration(seconds) * time.Second
		if err != nil || !slices.Contains(usageBucketSizes, query.Bucket) {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "bucket 必须是 auto、60、300、900、3600 或 86400")
			return
		}
	}
	if to.Sub(from)/query.Bucket > usageMaxBuckets {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("分桶数超过 %d，请选择更大的粒度", usageMaxBuckets))
		return
	}
	if value := values.Get("stack"); value != "" {
		if !slices.Contains(UsageDimensions, value) {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "stack 必须是 model、account、channel、protocol 或 state")
			return
		}
		query.Stack = value
	}
	report, err := s.config.Ledger.Usage(r.Context(), query)
	if err != nil {
		writeAdminError(w, ledgerErrorStatus(err), "usage_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// parseUsageRecordQuery 读取请求记录的范围、筛选、状态码、关键字与分页参数
func parseUsageRecordQuery(r *http.Request) (UsageRecordQuery, error) {
	values := r.URL.Query()
	from, to, err := parseUsageRange(values)
	if err != nil {
		return UsageRecordQuery{}, err
	}
	query := UsageRecordQuery{
		From: from, To: to, Filters: parseUsageFilters(values),
		Search: strings.TrimSpace(values.Get("q")), Limit: 50,
	}
	if value := values.Get("cursor"); value != "" {
		if query.Before, err = decodeUsageCursor(value); err != nil {
			return UsageRecordQuery{}, err
		}
	}
	if value := values.Get("status"); value != "" {
		if query.Status, err = strconv.Atoi(value); err != nil || query.Status < 100 || query.Status > 599 {
			return UsageRecordQuery{}, fmt.Errorf("status 必须是 HTTP 状态码")
		}
	}
	if value := values.Get("limit"); value != "" {
		if query.Limit, err = strconv.Atoi(value); err != nil || query.Limit < 1 || query.Limit > 200 {
			return UsageRecordQuery{}, fmt.Errorf("limit 必须是 1 到 200")
		}
	}
	return query, nil
}

func (s *server) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	query, err := parseUsageRecordQuery(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	page, err := s.config.Ledger.Records(r.Context(), query)
	if err != nil {
		writeAdminError(w, ledgerErrorStatus(err), "usage_records_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// exportWriter 在第一次写出时才设置下载响应头，查询失败时还能改为返回错误
type exportWriter struct {
	http.ResponseWriter
	name    string
	written bool
}

func (writer *exportWriter) Write(data []byte) (int, error) {
	if !writer.written {
		writer.written = true
		header := writer.Header()
		header.Set("Content-Type", "text/csv; charset=utf-8")
		header.Set("Content-Disposition", `attachment; filename="`+writer.name+`"`)
	}
	return writer.ResponseWriter.Write(data)
}

func (s *server) handleUsageExport(w http.ResponseWriter, r *http.Request) {
	query, err := parseUsageRecordQuery(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writer := &exportWriter{
		ResponseWriter: w,
		name:           fmt.Sprintf("usage-%s-%s.csv", query.From.Format("20060102T150405Z"), query.To.Format("20060102T150405Z")),
	}
	if err := s.config.Ledger.ExportRecords(r.Context(), query, writer); err != nil && !writer.written {
		writeAdminError(w, ledgerErrorStatus(err), "usage_export_failed", err.Error())
	}
}

func (s *server) handleRequestBody(w http.ResponseWriter, r *http.Request) {
	body, err := s.config.Ledger.RequestBody(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrRequestBodyNotFound) {
		writeAdminError(w, http.StatusNotFound, "request_body_not_found", err.Error())
		return
	}
	if err != nil {
		writeAdminError(w, ledgerErrorStatus(err), "request_body_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// bodyCapture 保存截断后的正文与原始字节数
type bodyCapture struct {
	mu   sync.Mutex
	data []byte
	size int64
}

func (capture *bodyCapture) add(data []byte) {
	capture.mu.Lock()
	capture.size += int64(len(data))
	if room := requestBodyLimit - len(capture.data); room > 0 {
		capture.data = append(capture.data, data[:min(room, len(data))]...)
	}
	capture.mu.Unlock()
}

func (capture *bodyCapture) result() (string, int64) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return string(capture.data), capture.size
}

// captureReader 在读取请求正文时复制前缀
type captureReader struct {
	io.ReadCloser
	capture *bodyCapture
}

func (reader *captureReader) Read(data []byte) (int, error) {
	count, err := reader.ReadCloser.Read(data)
	reader.capture.add(data[:count])
	return count, err
}

// captureWriter 在写出响应正文时复制前缀；刷新等能力经 Unwrap 交给内层
type captureWriter struct {
	http.ResponseWriter
	capture *bodyCapture
}

func (writer *captureWriter) Write(data []byte) (int, error) {
	count, err := writer.ResponseWriter.Write(data)
	writer.capture.add(data[:count])
	return count, err
}

func (writer *captureWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

// requestBodyMiddleware 在正文记录开启（REQUEST_BODY_LOG）时按请求日志 ID 保存 POST 请求与响应的截断正文。
// 放在 API key 校验之后：未通过校验的请求不保存；只保存正文，不保存请求头（API key、Cookie 都在请求头里）
func requestBodyMiddleware(ledger RequestLedger, next http.Handler) http.Handler {
	if ledger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || CountTokensPath(r.URL.Path) || !ledger.BodyCapture() {
			next.ServeHTTP(w, r)
			return
		}
		request, response := &bodyCapture{}, &bodyCapture{}
		r.Body = &captureReader{ReadCloser: r.Body, capture: request}
		next.ServeHTTP(&captureWriter{ResponseWriter: w, capture: response}, r)
		metadata, ok := r.Context().Value(accessLogContextKey{}).(*accessLogMetadata)
		if !ok {
			return
		}
		body := RequestBody{ID: metadata.id(), Time: time.Now().UTC()}
		body.Request, body.RequestSize = request.result()
		body.Response, body.ResponseSize = response.result()
		ledger.SaveBody(body)
	})
}
