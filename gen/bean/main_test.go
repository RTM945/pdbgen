package main

import (
	"bytes"
	"testing"

	"pdbgen/readxml"
)

func TestGenerateTableSupportsBoolAndFloats(t *testing.T) {
	schema := &readxml.Schema{
		Package: "ptable",
		Beans: []readxml.Bean{{
			Name: "TypeProbe",
			Variables: []readxml.Variable{
				{Name: "id", Type: "int64"},
				{Name: "flag", Type: "bool"},
				{Name: "ratio", Type: "float32"},
				{Name: "score", Type: "float64"},
			},
		}},
		Tables: []readxml.Table{{
			Name:       "type_probe",
			Bean:       "TypeProbe",
			PrimaryKey: &readxml.PrimaryKey{Variable: "id", AutoIncrement: true},
		}},
	}

	out, err := generateTable(schema, schema.Tables[0])
	if err != nil {
		t.Fatalf("generateTable failed: %v", err)
	}

	for _, want := range []string{
		"func (o *TypeProbe) SetFlag(v bool)",
		"func (o *TypeProbe) SetRatio(v float32)",
		"func (o *TypeProbe) SetScore(v float64)",
		"v.flag",
		"v.ratio",
		"v.score",
		"flag = $",
		"ratio = $",
		"score = $",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("generated source missing %q:\n%s", want, out)
		}
	}
}
