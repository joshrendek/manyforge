package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestPutRejectsEmptyPlaintext(t *testing.T) {
	_, err := (&Vault{}).Put(context.Background(), nil, uuid.New(), "connector", nil)
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}
