package api

import (
	"net/http/httptest"
	"testing"
)

func TestWriteDebug(t *testing.T) {
	rec := httptest.NewRecorder()
	rr := &responseRecorder{ResponseWriter: rec, status: 200}

	writeJSON(rr, 201, map[string]string{"id": "x"})

	t.Logf("buf len=%d content=%q recBody=%q", len(rr.buf.data), rr.buf.data, rec.Body.String())
	if len(rr.buf.data) == 0 {
		t.Fatal("recorder captured nothing")
	}
}
