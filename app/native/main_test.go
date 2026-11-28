package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/restic/restic/libackupnext/engine"
)

func testBuffer(size int) ([]byte, sharedBuffer) {
	data := make([]byte, size)
	return data, sharedBuffer{ptr: unsafe.Pointer(&data[0]), capacity: len(data)}
}

func TestFrameReadWrite(t *testing.T) {
	_, buf := testBuffer(4096)
	buf.writeFrameAt(0, []byte(`{"ok":true}`))
	got, err := buf.readRequest()
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("read = %q err=%v", got, err)
	}

	// 超长响应要变成一条 error JSON，而不是截断正文。
	buf.writeFrameAt(0, []byte(strings.Repeat("x", 5000)))
	got, err = buf.readRequest()
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(got, &response); err != nil {
		t.Fatalf("oversize response is not JSON: %v (%q)", err, got)
	}
	if ok, _ := response["ok"].(bool); ok {
		t.Fatalf("oversize response should have ok=false: %s", got)
	}

	// 请求帧长度非法。
	huge := make([]byte, 4096)
	binary.BigEndian.PutUint32(huge[:4], uint32(1<<30))
	invalid := sharedBuffer{ptr: unsafe.Pointer(&huge[0]), capacity: len(huge)}
	if _, err := invalid.readRequest(); err == nil {
		t.Fatalf("expected invalid length error")
	}
}

func TestProgressRing(t *testing.T) {
	_, buf := testBuffer(responseArea + progressArea)
	ring := newProgressRing(buf)
	ring.write(`{"stage":"a","state":"running"}`)
	ring.write(`{"stage":"a","state":"progress","total":1,"current":1}`)
	ring.write(`{"stage":"a","state":"done","files":1}`)

	raw := buf.bytes()
	counter := binary.BigEndian.Uint32(raw[progressOffset : progressOffset+progressCounterSize])
	if counter != 3 {
		t.Fatalf("counter = %d", counter)
	}
	for index := uint32(1); index <= 3; index++ {
		slot := int((index - 1) % uint32(progressSlotCount))
		offset := progressOffset + progressCounterSize + slot*progressSlotSize
		length := int(int32(binary.BigEndian.Uint32(raw[offset : offset+4])))
		text := string(raw[offset+4 : offset+4+length])
		if !strings.Contains(text, `"stage":"a"`) {
			t.Fatalf("slot %d = %q", index, text)
		}
	}
}

func TestRcloneLocalListDirs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b", "a", "c"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFileMain(t, filepath.Join(root, "file.txt"), "x")

	ensureRclone()
	fs, insecure, err := prepareFS(`{"type":"local","_root":"/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if insecure {
		t.Fatalf("local fs should not be insecure")
	}
	dirs, err := listDirs(fs, root, nil, insecure)
	if err != nil {
		t.Fatalf("listDirs: %v", err)
	}
	if !reflect.DeepEqual(dirs, []string{"a", "b", "c"}) {
		t.Fatalf("dirs = %v", dirs)
	}
}

func TestRcloneServeRestic(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	const password = "serve-test-password"
	if err := engine.InitRepo(&engine.InitRequest{Path: repoDir, Password: password, ForceInit: true}); err != nil {
		t.Fatalf("init repo: %v", err)
	}

	ensureRclone()
	fsJSON := `{"type":"local","_root":` + quoteJSON(repoDir) + `}`
	fs, insecure, err := prepareFS(fsJSON)
	if err != nil {
		t.Fatal(err)
	}
	url, err := remoteURL(fs, "local|"+fsJSON, insecure)
	if err != nil {
		t.Fatalf("ensure serve: %v", err)
	}
	if !strings.HasPrefix(url, "http://") || !strings.Contains(url, "@127.0.0.1:") {
		t.Fatalf("unexpected url: %s", url)
	}
	t.Cleanup(func() {
		stopServe()
		engine.SetRepo("", "", nil)
	})

	if err := engine.InitRepo(&engine.InitRequest{Path: "rest:" + url, Password: password}); err != nil {
		t.Fatalf("open restic over serve: %v", err)
	}
	engine.SetRepo("rest:"+url, password, nil)
	snapshots, err := engine.ListSnapshots("")
	if err != nil {
		t.Fatalf("list over serve: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("new repo should have no snapshots: %+v", snapshots)
	}

	layout, err := engine.RepoLayout("rest:"+url, nil)
	if err != nil {
		t.Fatalf("layout over serve: %v", err)
	}
	if !layout.Config {
		t.Fatalf("rest layout should report config: %+v", layout)
	}

	// 复用同一个 key 不应重新起服务。
	again, err := remoteURL(fs, "local|"+fsJSON, insecure)
	if err != nil || again != url {
		t.Fatalf("serve should be reused: %q err=%v", again, err)
	}
}

func TestPrepareFS(t *testing.T) {
	fs, insecure, err := prepareFS(`{"type":"webdav","_root":"/repo","pass":"plain","insecure":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !insecure {
		t.Fatalf("insecure not returned")
	}
	if _, ok := fs["insecure"]; ok {
		t.Fatalf("insecure should be stripped from fs config")
	}
	pass, _ := fs["pass"].(string)
	if pass == "" || pass == "plain" {
		t.Fatalf("pass was not obscured: %q", pass)
	}
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func writeFileMain(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
