package clickhouse

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

type captureNATConn struct {
	driver.Conn
	batch captureNATBatch
	query string
}

func (c *captureNATConn) PrepareBatch(_ context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.query = query
	return &c.batch, nil
}

type captureNATBatch struct {
	driver.Batch
	rows [][]any
}

func (b *captureNATBatch) Append(values ...any) error {
	b.rows = append(b.rows, values)
	return nil
}
func (b *captureNATBatch) Send() error { return nil }

func TestWriterPersistsPostNATDestinationAndLegacyDefaults(t *testing.T) {
	conn := &captureNATConn{}
	mgr, _ := testManager(config.DropNew)
	mgr.conn, mgr.insert = conn, fmt.Sprintf(insertStmt, "natlogs")
	rows := []normalizer.FlowRecord{{
		SrcIP: net.ParseIP("57.144.140.3"), SrcPort: 443,
		DstIP: net.ParseIP("151.158.226.176"), DstPort: 42286,
		NatPublicIP: net.ParseIP("57.144.140.3"), NatPublicPort: 443,
		NatDestIP: net.ParseIP("10.0.102.12"), NatDestPort: 42286,
	}, {NatPublicIP: net.ParseIP("203.0.113.1"), NatPublicPort: 1234}}
	if err := (&shard{mgr: mgr}).send(rows); err != nil {
		t.Fatal(err)
	}
	columns := strings.Split(strings.TrimSuffix(strings.SplitN(conn.query, "(", 2)[1], ")"), ",")
	if len(conn.batch.rows) != 2 {
		t.Fatalf("writer inserted %d rows", len(conn.batch.rows))
	}
	for rowIndex, got := range conn.batch.rows {
		if len(got) != len(columns) {
			t.Fatalf("insert column/value mismatch: %d / %d", len(columns), len(got))
		}
		values := map[string]any{}
		for i, column := range columns {
			values[strings.TrimSpace(column)] = got[i]
		}
		wantIP, wantPort := "10.0.102.12", uint16(42286)
		if rowIndex == 1 {
			wantIP, wantPort = "0.0.0.0", 0
		}
		if values["nat_dest_ip"].(net.IP).String() != wantIP || values["nat_dest_port"] != wantPort {
			t.Fatalf("post-NAT destination lost at insert: %+v", values)
		}
		if !values["nat_public_ip"].(net.IP).Equal(rows[rowIndex].NatPublicIP) || values["nat_public_port"] != rows[rowIndex].NatPublicPort {
			t.Fatal("writer changed the historical post-source fields")
		}
	}
}
