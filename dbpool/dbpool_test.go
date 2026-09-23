package dbpool

import (
	"context"
	"pdbgen/readxml"
	"testing"
)

func TestPG(t *testing.T) {
	schema, err := readxml.LoadSchema("../pdb.xml")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = Init(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := dbpool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}
