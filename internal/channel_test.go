package internal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cfgWith builds a Config with the given profile aliases (no per-profile overrides).
func cfgWith(aliases ...string) *Config {
	c := &Config{Profiles: map[string]*Profile{}}
	for _, a := range aliases {
		c.Profiles[a] = &Profile{}
	}
	return c
}

func TestChannelPortIsDeterministic(t *testing.T) {
	cfg := cfgWith("bdaya", "digrum", "digrum1", "glm", "gmail")
	first, err := ChannelPort(cfg, "digrum1")
	if err != nil {
		t.Fatalf("ChannelPort: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := ChannelPort(cfg, "digrum1")
		if err != nil {
			t.Fatalf("ChannelPort (repeat %d): %v", i, err)
		}
		if again != first {
			t.Fatalf("port not stable across calls: %d then %d", first, again)
		}
	}
}

func TestChannelPortNoCollisions(t *testing.T) {
	aliases := []string{"bdaya", "digrum", "digrum1", "glm", "gmail"}
	cfg := cfgWith(aliases...)
	seen := map[int]string{}
	for _, a := range aliases {
		p, err := ChannelPort(cfg, a)
		if err != nil {
			t.Fatalf("ChannelPort(%s): %v", a, err)
		}
		if other, dup := seen[p]; dup {
			t.Fatalf("port %d assigned to both %q and %q", p, other, a)
		}
		seen[p] = a
	}
}

func TestChannelPortExplicitOverrideWins(t *testing.T) {
	cfg := cfgWith("bdaya", "digrum1")
	cfg.Profiles["digrum1"].ChannelPort = 9999
	got, err := ChannelPort(cfg, "digrum1")
	if err != nil {
		t.Fatalf("ChannelPort: %v", err)
	}
	if got != 9999 {
		t.Fatalf("explicit channel_port ignored: want 9999, got %d", got)
	}
}

func TestChannelPortHonorsBaseOverride(t *testing.T) {
	cfg := cfgWith("solo")
	cfg.ChannelBasePort = 9100
	got, err := ChannelPort(cfg, "solo")
	if err != nil {
		t.Fatalf("ChannelPort: %v", err)
	}
	if got != 9100 {
		t.Fatalf("base override ignored: want 9100, got %d", got)
	}
}

// An unknown alias must fail LOUDLY and name the known aliases: a silent default
// would POST a message into some other profile's session.
func TestChannelPortUnknownAliasIsActionable(t *testing.T) {
	cfg := cfgWith("bdaya", "digrum1")
	_, err := ChannelPort(cfg, "nope")
	if err == nil {
		t.Fatal("unknown alias must error, not fall back to a default port")
	}
	msg := err.Error()
	for _, want := range []string{"nope", "bdaya", "digrum1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must name the bad alias and the known ones; got %q", msg)
		}
	}
}

func TestSendToProfilePostsTheExactBody(t *testing.T) {
	var gotBody, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := portOfTestServer(t, srv.URL)
	cfg := cfgWith("solo")
	cfg.Profiles["solo"].ChannelPort = port

	const msg = "run: echo CHANNEL_SEND_OK"
	if err := SendToProfile(cfg, "solo", msg); err != nil {
		t.Fatalf("SendToProfile: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("want POST, got %s", gotMethod)
	}
	// The channel server's trigger is /push; / is the MCP-less 404 path. Posting to
	// the wrong path silently reaches nobody, so this is pinned.
	if gotPath != "/push" {
		t.Fatalf("want POST to /push (the channel trigger), got %s", gotPath)
	}
	if gotBody != msg {
		t.Fatalf("body mangled: want %q, got %q", msg, gotBody)
	}
}

// A channel that answers non-2xx means the message did NOT reach the session;
// reporting success there would silently drop fleet work.
func TestSendToProfileErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := cfgWith("solo")
	cfg.Profiles["solo"].ChannelPort = portOfTestServer(t, srv.URL)
	if err := SendToProfile(cfg, "solo", "hi"); err == nil {
		t.Fatal("non-2xx must be an error, not a silent success")
	}
}

func TestSendToProfileErrorsWhenNothingListening(t *testing.T) {
	cfg := cfgWith("solo")
	cfg.Profiles["solo"].ChannelPort = 1 // nothing binds port 1
	if err := SendToProfile(cfg, "solo", "hi"); err == nil {
		t.Fatal("an unreachable channel must error")
	}
}

// Status must distinguish a bound port from a dead one: a killed session can leave
// its channel subprocess holding the port, and an operator needs to see that.
func TestChannelStatusReportsListening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := cfgWith("live", "dead")
	cfg.Profiles["live"].ChannelPort = portOfTestServer(t, srv.URL)
	cfg.Profiles["dead"].ChannelPort = 1

	states := ChannelStatus(cfg)
	if len(states) != 2 {
		t.Fatalf("want 2 states, got %d", len(states))
	}
	byAlias := map[string]ChannelState{}
	for _, s := range states {
		byAlias[s.Alias] = s
	}
	if !byAlias["live"].Listening {
		t.Fatal("a bound port must report Listening=true")
	}
	if byAlias["dead"].Listening {
		t.Fatal("an unbound port must report Listening=false")
	}
}

// ChannelStatus must be sorted so repeated runs diff cleanly.
func TestChannelStatusIsSorted(t *testing.T) {
	cfg := cfgWith("zeta", "alpha", "mid")
	states := ChannelStatus(cfg)
	for i := 1; i < len(states); i++ {
		if states[i-1].Alias > states[i].Alias {
			t.Fatalf("states not sorted: %q before %q", states[i-1].Alias, states[i].Alias)
		}
	}
}

// portOfTestServer extracts the numeric port from an httptest server URL.
func portOfTestServer(t *testing.T, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	if i < 0 {
		t.Fatalf("cannot parse test server URL %q", url)
	}
	var p int
	if _, err := fmt.Sscanf(url[i+1:], "%d", &p); err != nil {
		t.Fatalf("cannot parse port from %q: %v", url, err)
	}
	return p
}

// install wires the profile's sessions to their inbox: without it `cpm channel send`
// has nothing to deliver to. The entry MUST be type:"http" pointing at /mcp — an
// HTTP channel is not a session subprocess, so ONE server serves every session of
// the profile (proven 2026-07-24). It MUST NOT be routed via a proxy: a proxied
// channel strips the capability and silently discards every push.
func TestChannelInstallWritesHttpEntry(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWith("solo")
	cfg.Profiles["solo"].ChannelPort = 8888

	if err := ChannelInstall(cfg, dir, "solo"); err != nil {
		t.Fatalf("ChannelInstall: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "solo", ".claude.json"))
	if err != nil {
		t.Fatalf("read profile config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profile config is not valid JSON: %v", err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	entry, _ := servers[ChannelServerName].(map[string]any)
	if entry == nil {
		t.Fatalf("no %q entry written; got %v", ChannelServerName, servers)
	}
	if entry["type"] != "http" {
		t.Fatalf("channel entry must be type:http (stdio would bind it to ONE session), got %v", entry["type"])
	}
	if got := entry["url"]; got != "http://127.0.0.1:8888/mcp" {
		t.Fatalf("wrong url: %v", got)
	}
}

// Installing MUST NOT clobber the profile's existing MCP servers.
func TestChannelInstallPreservesExistingServers(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "solo"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"mcpServers":{"keepme":{"type":"http","url":"http://example/mcp"}},"other":42}`
	if err := os.WriteFile(filepath.Join(dir, "solo", ".claude.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWith("solo")
	cfg.Profiles["solo"].ChannelPort = 8888
	if err := ChannelInstall(cfg, dir, "solo"); err != nil {
		t.Fatalf("ChannelInstall: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "solo", ".claude.json"))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid JSON after install: %v", err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers["keepme"] == nil {
		t.Fatal("install clobbered an existing MCP server")
	}
	if doc["other"] == nil {
		t.Fatal("install dropped an unrelated top-level key")
	}
}

// The channel server answers 200 even when NO session is connected -- the POST
// reached the server, but nobody received the message. Treating that as success is
// the same decoupled-signal bug as an exit-0 failure: the caller believes work was
// dispatched that nobody will ever do.
func TestSendToProfileErrorsWhenNoSessionReceived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"sessionCount":0,"results":[]}`))
	}))
	defer srv.Close()

	cfg := cfgWith("solo")
	cfg.Profiles["solo"].ChannelPort = portOfTestServer(t, srv.URL)
	err := SendToProfile(cfg, "solo", "hi")
	if err == nil {
		t.Fatal("a push nobody received must NOT report success")
	}
	if !strings.Contains(err.Error(), "no session") {
		t.Fatalf("error must say nobody was listening; got %q", err)
	}
}

// ...but a delivered push stays a success.
func TestSendToProfileSucceedsWhenASessionReceived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"sessionCount":2,"results":[]}`))
	}))
	defer srv.Close()

	cfg := cfgWith("solo")
	cfg.Profiles["solo"].ChannelPort = portOfTestServer(t, srv.URL)
	if err := SendToProfile(cfg, "solo", "hi"); err != nil {
		t.Fatalf("a delivered push must succeed: %v", err)
	}
}
