// sqlrun: executor mínimo de batches T-SQL, substituto do go-sqlcmd.
//
// O go-sqlcmd embute cliente Docker, SSH e SDK Azure (features de "sqlcmd
// create") e carrega dezenas de CVEs dessas dependências que nunca usamos.
// Aqui só existe o que o driver mssql.sh precisa: conectar via TDS, rodar um
// batch, imprimir linhas (sem cabeçalho, colunas separadas por tab), repassar
// as mensagens informativas do servidor e sair com código != 0 em erro.
//
// Conexão via env (MSSQL_HOST, MSSQL_PORT, MSSQL_USER, MSSQL_PASSWORD — a
// senha nunca passa por argv — e MSSQL_CA_FILE opcional). Flags: -t <segundos> e -Q <batch>.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-sql/sqlexp"
	_ "github.com/microsoft/go-mssqldb"
)

func main() {
	timeout := flag.Int("t", 0, "timeout do batch em segundos (0 = sem timeout)")
	query := flag.String("Q", "", "batch T-SQL a executar")
	flag.Parse()

	if *query == "" {
		fmt.Fprintln(os.Stderr, "uso: sqlrun [-t segundos] -Q <batch>")
		os.Exit(2)
	}

	host := os.Getenv("MSSQL_HOST")
	pass := os.Getenv("MSSQL_PASSWORD")
	if host == "" || pass == "" {
		fmt.Fprintln(os.Stderr, "sqlrun: MSSQL_HOST/MSSQL_PASSWORD não definidas")
		os.Exit(2)
	}
	port := os.Getenv("MSSQL_PORT")
	if port == "" {
		port = "1433"
	}
	user := os.Getenv("MSSQL_USER")
	if user == "" {
		user = "sa"
	}

	params := url.Values{
		"app name":     {"db-backup"},
		"dial timeout": {"15"},
	}
	// Com MSSQL_CA_FILE o certificado do servidor é validado e a sessão inteira
	// é cifrada — sem isso um MITM na rede captura a senha no login.
	// ponytail: sem CA cai no trustservercertificate (compatível com o cert
	// autoassinado padrão do SQL Server, como o -C do sqlcmd), que não protege
	// contra MITM; o upgrade é sempre definir MSSQL_CA_FILE.
	if ca := os.Getenv("MSSQL_CA_FILE"); ca != "" {
		params.Set("encrypt", "true")
		params.Set("certificate", ca)
	} else {
		params.Set("trustservercertificate", "true")
	}
	dsn := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(user, pass),
		Host:     net.JoinHostPort(host, port),
		RawQuery: params.Encode(),
	}

	db, err := sql.Open("sqlserver", dsn.String())
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqlrun:", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*timeout)*time.Second)
		defer cancel()
	}

	if err := run(ctx, db, *query); err != nil {
		fmt.Fprintln(os.Stderr, "sqlrun:", err)
		os.Exit(1)
	}
}

// Executa o batch consumindo o stream de mensagens do servidor (sqlexp é o
// mecanismo do go-mssqldb para receber PRINT e os "Processed N pages" do
// BACKUP/RESTORE junto com as linhas de resultado).
func run(ctx context.Context, db *sql.DB, query string) error {
	retmsg := &sqlexp.ReturnMessage{}
	rows, err := db.QueryContext(ctx, query, retmsg)
	if err != nil {
		return err
	}
	defer rows.Close()

	var batchErr error
	for active := true; active; {
		switch m := retmsg.Message(ctx).(type) {
		case sqlexp.MsgNotice:
			fmt.Println(m.Message)
		case sqlexp.MsgError:
			batchErr = m.Error
		case sqlexp.MsgRowsAffected:
			fmt.Printf("(%d rows affected)\n", m.Count)
		case sqlexp.MsgNextResultSet:
			active = rows.NextResultSet()
		case sqlexp.MsgNext:
			if err := printRows(rows); err != nil {
				batchErr = err
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("batch abortado: %w", ctx.Err())
		}
	}
	if err := rows.Err(); err != nil && batchErr == nil {
		batchErr = err
	}
	return batchErr
}

func printRows(rows *sql.Rows) error {
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if v == nil {
				parts[i] = "NULL"
			} else {
				parts[i] = fmt.Sprintf("%v", v)
			}
		}
		fmt.Println(strings.Join(parts, "\t"))
	}
	return nil
}
