package httpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreCache "github.com/logoscore/logos-golang-channel-core/pkg/cache"
	coreMatcher "github.com/logoscore/logos-golang-channel-core/pkg/matcher"
	coreProfile "github.com/logoscore/logos-golang-channel-core/pkg/profile"
	coreRuntime "github.com/logoscore/logos-golang-channel-core/pkg/runtime"
	coreSync "github.com/logoscore/logos-golang-channel-core/pkg/syncclient"
)

type fieldSet struct {
	location, key, value string
}

type envelope struct {
	data map[string]string
	sets []fieldSet
}

func (e *envelope) SourceKey() string { return "http" }
func (e *envelope) GetField(location, key string) (string, bool) {
	v, ok := e.data[location+"."+key]
	if ok {
		return v, true
	}
	// Fallback: case-insensitive lookup (handles Go's canonical header casing
	// vs profile-defined header keys like "X-Request-ID" → "X-Request-Id").
	v, ok = e.data[location+"."+strings.ToLower(key)]
	return v, ok
}
func (e *envelope) SetField(location, key, value string) {
	e.data[location+"."+key] = value
	e.sets = append(e.sets, fieldSet{location, key, value})
}

func newEnvelopeFromRequest(r *http.Request, body map[string]any, rawBody string) *envelope {
	env := &envelope{data: map[string]string{}}
	if body != nil {
		for k, v := range body {
			env.data["body."+k] = fmt.Sprintf("%v", v)
		}
	}
	env.data["body."] = rawBody
	for k, v := range r.Header {
		if len(v) > 0 {
			env.data["header."+k] = v[0]
			env.data["header."+strings.ToLower(k)] = v[0]
		}
	}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			env.data["query."+k] = v[0]
		}
	}
	for _, c := range r.Cookies() {
		env.data["cookie."+c.Name] = c.Value
	}
	return env
}

func cloneEnvelope(src *envelope) *envelope {
	dst := &envelope{data: make(map[string]string, len(src.data))}
	for k, v := range src.data {
		dst.data[k] = v
	}
	return dst
}

func detectHintID(env *envelope, profiles []coreProfile.Profile) int32 {
	for _, ref := range []struct{ loc, key string }{
		{"query", "profile_id"},
		{"header", "X-Profile-ID"},
		{"header", "x-profile-id"},
		{"cookie", "profile_id"},
		{"body", "profile_id"},
	} {
		if v, ok := env.GetField(ref.loc, ref.key); ok && v != "" {
			if id, err := strconv.ParseInt(v, 10, 32); err == nil {
				return int32(id)
			}
		}
	}
	for _, p := range profiles {
		if p.Mapping.ProfileID == nil {
			continue
		}
		loc := strings.TrimSpace(p.Mapping.ProfileID.Target.Location)
		key := strings.TrimSpace(p.Mapping.ProfileID.Target.Key)
		if v, ok := env.GetField(loc, key); ok && v != "" {
			if id, err := strconv.ParseInt(v, 10, 32); err == nil {
				return int32(id)
			}
		}
	}
	return 0
}

func writeHTTPResponse(w http.ResponseWriter, env *envelope, p coreProfile.Profile) {
	// Write noise headers and cookies set by runtime
	for _, s := range env.sets {
		switch strings.ToLower(s.location) {
		case "header":
			w.Header().Set(s.key, s.value)
		case "cookie":
			http.SetCookie(w, &http.Cookie{Name: s.key, Value: s.value, Path: "/"})
		}
	}

	outLoc := strings.TrimSpace(strings.ToLower(p.Mapping.EncryptedDataOut.Target.Location))
	outKey := strings.TrimSpace(p.Mapping.EncryptedDataOut.Target.Key)
	encOut, _ := env.GetField(p.Mapping.EncryptedDataOut.Target.Location, outKey)

	if outLoc == "body" && outKey == "" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(encOut))
		return
	}

	if outLoc == "header" {
		w.Header().Set(outKey, encOut)
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		outKey: encOut,
	})
}

type profilesProvider interface{ Profiles() []coreProfile.Profile }

type staticProfiles []coreProfile.Profile

func (s staticProfiles) Profiles() []coreProfile.Profile {
	return append([]coreProfile.Profile(nil), s...)
}

func New(addr, channelID, c2SyncBaseURL string, profiles []coreProfile.Profile) *http.Server {
	return NewWithProvider(addr, channelID, c2SyncBaseURL, staticProfiles(profiles))
}

func NewWithProvider(addr, channelID, c2SyncBaseURL string, provider profilesProvider) *http.Server {
	runtime := coreRuntime.New(coreSync.NewHTTPClient(c2SyncBaseURL, nil))
	affinity := coreCache.NewAffinity(10 * time.Minute)
	matcher := coreMatcher.NewWithCache(affinity)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rawBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		rawBody := string(rawBytes)
		var body map[string]any
		_ = json.Unmarshal(rawBytes, &body)

		baseEnv := newEnvelopeFromRequest(r, body, rawBody)

		profiles := provider.Profiles()
		hintID := detectHintID(baseEnv, profiles)

		candidates := matcher.EnabledOrdered(profiles)
		if res, err := matcher.Resolve(r.Context(), r.RemoteAddr, hintID, candidates); err == nil {
			candidates = []coreProfile.Profile{res.Profile}
		}

		var lastErr error
		for _, p := range candidates {
			env := cloneEnvelope(baseEnv)
			_, err := runtime.HandleWithProfile(r.Context(), env, channelID, p)
			if err != nil {
				lastErr = err
				continue
			}
			matcher.RecordMatch(r.RemoteAddr, p.ProfileID)
			writeHTTPResponse(w, env, p)
			return
		}

		http.Error(w, "profile resolution failed: unmatched payload", http.StatusBadRequest)
		_ = lastErr
	})

	return &http.Server{Addr: addr, Handler: mux}
}
