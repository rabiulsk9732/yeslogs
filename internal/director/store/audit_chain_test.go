package store

import (
	"context"
	"testing"
	"time"
)

func TestMemAuditChain(t *testing.T) {
	m := NewMem()
	q := QueryAudit{UserEmail: "a@example.test", ISPID: 2, QueryIP: "203.0.113.4", FromTS: time.Unix(1, 0), ToTS: time.Unix(2, 0)}
	if _, e := m.LogQuery(context.Background(), q); e != nil {
		t.Fatal(e)
	}
	if _, e := m.LogQuery(context.Background(), q); e != nil {
		t.Fatal(e)
	}
	if n, e := m.VerifyAuditChain(context.Background()); e != nil || n != 2 {
		t.Fatalf("%d %v", n, e)
	}
	m.queries[0].CaseRef = "tampered"
	if _, e := m.VerifyAuditChain(context.Background()); e == nil {
		t.Fatal("tampering not detected")
	}
}
