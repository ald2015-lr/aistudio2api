package requestdb

import (
	"database/sql"
	"errors"
	"fmt"
)

// migrate 把旧版账本升级到当前结构，每次打开都执行，可以重复执行：
// requests 缺少 pool 列时用 ALTER TABLE 追加（旧记录为空，即普通号池），并建立按号池与时间的索引；
// 小时与本地日汇总的主键要加入 pool，SQLite 不能修改主键，缺少 pool 列时在一个事务内按新结构重建表并复制旧汇总
func migrate(db *sql.DB, log Logger) error {
	added, err := addColumn(db, "requests", "pool", `ALTER TABLE requests ADD COLUMN pool TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS requests_pool_time ON requests(pool, time)`); err != nil {
		return err
	}
	rebuilt := 0
	for _, table := range []string{"hours", "days"} {
		done, err := rebuildRollupWithPool(db, table)
		if err != nil {
			return fmt.Errorf("汇总表 %s 加入号池列: %w", table, err)
		}
		if done {
			rebuilt++
		}
	}
	if added || rebuilt > 0 {
		log("INFO", fmt.Sprintf("请求账本已加入号池列 | 原始记录=%t | 重建汇总表=%d", added, rebuilt))
	}
	return nil
}

// hasColumn 判断表是否已有该列
func hasColumn(db *sql.DB, table string, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, errors.Join(err, rows.Close())
		}
		found = found || name == column
	}
	return found, errors.Join(rows.Err(), rows.Close())
}

// addColumn 在表缺少该列时执行 statement 追加，返回是否追加
func addColumn(db *sql.DB, table string, column string, statement string) (bool, error) {
	exists, err := hasColumn(db, table, column)
	if err != nil || exists {
		return false, err
	}
	if _, err := db.Exec(statement); err != nil {
		return false, err
	}
	return true, nil
}

// rebuildRollupWithPool 在汇总表缺少 pool 列时按当前结构重建（主键加入 pool），旧汇总全部归入普通号池，返回是否重建
func rebuildRollupWithPool(db *sql.DB, table string) (rebuilt bool, err error) {
	exists, err := hasColumn(db, table, "pool")
	if err != nil || exists {
		return false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	legacy := table + "_before_pool"
	for _, statement := range []string{
		`DROP TABLE IF EXISTS ` + legacy,
		`ALTER TABLE ` + table + ` RENAME TO ` + legacy,
		`CREATE TABLE ` + table + ` (` + rollupColumns + `) WITHOUT ROWID`,
		`INSERT INTO ` + table + ` (` + rollupColumnsV1 + `, pool) SELECT ` + rollupColumnsV1 + `, '' FROM ` + legacy,
		`DROP TABLE ` + legacy,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
