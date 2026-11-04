package insights

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

const testViewerKey = "insights-viewer-key-0123456789abcdef"

func setViewerKey(t *testing.T, key string) {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	t.Setenv(viewerKeyEnv, hex.EncodeToString(sum[:]))
}

func TestViewerKeyMatches(t *testing.T) {
	t.Setenv(viewerKeyEnv, "")
	if ViewerKeyMatches(testViewerKey) {
		t.Fatal("matched with no hash configured")
	}

	t.Setenv(viewerKeyEnv, "not-hex")
	if ViewerKeyMatches(testViewerKey) {
		t.Fatal("matched with an invalid hash")
	}

	setViewerKey(t, testViewerKey)
	if !ViewerKeyMatches(testViewerKey) {
		t.Fatal("configured key did not match")
	}
	if ViewerKeyMatches(testViewerKey + "x") {
		t.Fatal("different key matched")
	}

	short := "short-key"
	setViewerKey(t, short)
	if ViewerKeyMatches(short) {
		t.Fatal("key shorter than the minimum matched")
	}
}

func TestStatusHidesDatabasePathFromViewer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, errOpen := OpenStore(filepath.Join(t.TempDir(), "insights.db"))
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	defer func() { _ = store.Close() }()
	svc := &Service{store: store, recorder: newRecorder(store)}
	current.Store(svc)
	defer current.CompareAndSwap(svc, nil)

	for _, tc := range []struct {
		viewer   bool
		access   string
		wantPath bool
	}{
		{viewer: false, access: "management", wantPath: true},
		{viewer: true, access: "viewer", wantPath: false},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/insights/status", nil)
		if tc.viewer {
			c.Set(ViewerAccessContextKey, true)
		}
		handleStatus(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("viewer=%v: status=%d body=%s", tc.viewer, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil {
			t.Fatalf("decode: %v", errDecode)
		}
		if body["access"] != tc.access {
			t.Fatalf("viewer=%v: access=%v want %s", tc.viewer, body["access"], tc.access)
		}
		if _, has := body["database_path"]; has != tc.wantPath {
			t.Fatalf("viewer=%v: database_path present=%v want %v", tc.viewer, has, tc.wantPath)
		}
	}
}
