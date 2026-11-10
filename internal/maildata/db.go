package maildata

import (
	"database/sql"
	"net/url"

	_ "modernc.org/sqlite"
)

// openReadOnly opens a SQLite file strictly read-only.
func openReadOnly(path string) (*sql.DB, error) {
	u := url.URL{Path: path}
	dsn := "file:" + u.EscapedPath() + "?mode=ro&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
