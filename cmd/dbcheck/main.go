// Command dbcheck verifies that the MySQL database is reachable.
// Temporary helper; removed once the real CLI exists.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const defaultDSN = "admin:admin@tcp(127.0.0.1:3307)/coderpush_db?parseTime=true&loc=UTC&charset=utf8mb4"

func main() {
	dsn := os.Getenv("CATALOG_DSN")
	if dsn == "" {
		dsn = defaultDSN
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	fmt.Println("MySQL version:", version)
}
