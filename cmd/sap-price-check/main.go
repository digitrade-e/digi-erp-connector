// Command sap-price-check proves the connector's SAP Business One pricing on a client's own data
// before go-live: it prices the sales-order lines users entered in the SAP client over the last days
// through the connector's engine and counts, per price source, how often the base price, the
// discount and the final price equal the ones SAP itself put on the line (internal/erp/sap.Check).
//
//	sap-price-check -database <company DB> [-server host[\instance]] [-port 1433] [-user login] [-days 30] [-groups 300]
//
// The password of -user is read from SAP_CHECK_PASSWORD (never a flag); with no -user the check logs
// in with Windows authentication. It only reads, and prints counts only — no customer, item or
// price — as JSON. Exit 0: the check ran; 1: it could not; 2: usage. Never released.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/sap"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sap-price-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "localhost", `SQL Server host, or host\instance`)
	port := fs.Int("port", 1433, "SQL Server port (ignored with an instance name)")
	database := fs.String("database", "", "the SAP company database")
	user := fs.String("user", "", "SQL login (password in SAP_CHECK_PASSWORD); empty = Windows authentication")
	days := fs.Int("days", 30, "look-back in days")
	groups := fs.Int("groups", 300, "at most this many (customer, date) groups are priced")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || strings.TrimSpace(*database) == "" {
		fmt.Fprintln(stderr, "usage: sap-price-check -database <company DB> [-server host] [-port 1433] [-user login] [-days 30] [-groups 300]")
		return 2
	}
	db, err := sql.Open("sqlserver", dsn(*server, *port, *database, *user, os.Getenv("SAP_CHECK_PASSWORD")))
	if err != nil {
		fmt.Fprintln(stderr, "open:", err)
		return 1
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	rep, err := sap.Check(ctx, db, sap.CheckOptions{Days: *days, MaxGroups: *groups})
	if err != nil {
		fmt.Fprintln(stderr, "check:", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return 1
	}
	return 0
}

func dsn(server string, port int, database, user, password string) string {
	host, instance, _ := strings.Cut(strings.TrimSpace(server), `\`)
	u := &url.URL{Scheme: "sqlserver", Host: host}
	if instance != "" {
		u.Path = instance
	} else {
		u.Host = host + ":" + strconv.Itoa(port)
	}
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	q := url.Values{}
	q.Set("database", database)
	q.Set("app name", "sap-price-check")
	q.Set("ApplicationIntent", "ReadOnly")
	u.RawQuery = q.Encode()
	return u.String()
}
