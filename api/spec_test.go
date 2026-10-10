package api

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestSpecValid(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
