package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"path/filepath"
	"testing"
)

func TestCLIPublishDeleteFreshSync(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	gallery := filepath.Join(root, "gallery.git")
	invoke := func(state string, input []byte, args ...string) map[string]any {
		t.Helper()
		var out, stderr bytes.Buffer
		argv := append([]string{"--state-dir", state, "--json"}, args...)
		exit := Run(context.Background(), argv, bytes.NewReader(input), &out, &stderr)
		if exit != 0 {
			t.Fatalf("%v exit %d: %s", args, exit, stderr.String())
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err, out.String())
		}
		return result
	}
	invoke(a, nil, "repo", "init", "--local", gallery)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	added := invoke(a, data.Bytes(), "add", "-", "--title", "demo")
	id := added["assets"].([]any)[0].(map[string]any)["asset"].(map[string]any)["id"].(string)
	first := invoke(a, nil, "commit", "-m", "add")
	if first["change_count"] != float64(1) || first["asset_count"] != float64(1) {
		t.Fatal(first)
	}
	verify := invoke(a, nil, "verify")
	if verify["assets"] != float64(1) || verify["objects"] != float64(1) {
		t.Fatal(verify)
	}
	invoke(b, nil, "repo", "use", "--local", gallery)
	invoke(b, nil, "sync")
	fresh := invoke(b, nil, "ls")
	if fresh["assets"].([]any)[0].(map[string]any)["asset"].(map[string]any)["id"] != id {
		t.Fatal(fresh)
	}
	empty := invoke(a, nil, "commit", "-m", "empty")
	if empty["head"] != first["head"] || empty["change_count"] != float64(0) {
		t.Fatal(empty)
	}
	invoke(a, nil, "rm", id)
	invoke(a, nil, "commit", "-m", "remove")
	invoke(b, nil, "sync")
	if len(invoke(b, nil, "ls")["assets"].([]any)) != 0 {
		t.Fatal("deleted asset survived fresh sync")
	}
	verify = invoke(b, nil, "verify")
	if verify["objects"] != float64(0) {
		t.Fatal(verify)
	}
}
