package internal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	var gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
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
