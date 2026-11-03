package server

import (
	"crypto/sha256"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iamgp/tideline/internal/auth"
)

// authLimiter slows down credential guessing. Each client IP has a leaky
// bucket of failed sign-ins: it holds Burst failures and drains at one per
// Refill. A full bucket blocks every request from that IP until it drains.
//
// The drain rate is chosen so a device left with an old token (an uploader
// retrying every few minutes) never fills its bucket, while guessing is held
// to about one attempt per minute per IP. Successful sign-ins don't empty
// the bucket, so a valid token can't be used to reset an attack.
type authLimiter struct {
	Burst  float64
	Refill time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	sweep   time.Time
	known   map[[32]byte]time.Time // credentials that have succeeded recently
}

// knownTTL is how long a credential that worked is trusted past a block.
const knownTTL = 30 * 24 * time.Hour

func fingerprint(cred string) [32]byte { return sha256.Sum256([]byte(cred)) }

// Succeeded remembers a credential that authenticated, so devices already set
// up keep working when their IP is blocked (for example a shared carrier or
// proxy address that an attacker is also using).
func (l *authLimiter) Succeeded(cred string) {
	if cred == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.known) >= 10000 {
		now := l.now()
		for k, t := range l.known {
			if now.Sub(t) > knownTTL {
				delete(l.known, k)
			}
		}
		if len(l.known) >= 10000 {
			return
		}
	}
	l.known[fingerprint(cred)] = l.now()
}

// Known reports whether a credential has authenticated recently.
func (l *authLimiter) Known(cred string) bool {
	if cred == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.known[fingerprint(cred)]
	return ok && l.now().Sub(t) <= knownTTL
}

type bucket struct {
	level float64
	at    time.Time
}

func newAuthLimiter(burst int, refill time.Duration) *authLimiter {
	return &authLimiter{Burst: float64(burst), Refill: refill, now: time.Now,
		buckets: map[string]*bucket{}, known: map[[32]byte]time.Time{}}
}

// drain brings a bucket up to date. Callers hold l.mu.
func (l *authLimiter) drain(b *bucket, now time.Time) {
	b.level = math.Max(0, b.level-float64(now.Sub(b.at))/float64(l.Refill))
	b.at = now
}

// Blocked returns how long the IP must wait, or 0 if it may try.
func (l *authLimiter) Blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		return 0
	}
	l.drain(b, l.now())
	// Blocked from the Burst-th failure until one failure has drained.
	over := b.level - (l.Burst - 1)
	if over <= 0 {
		return 0
	}
	return time.Duration(over * float64(l.Refill))
}

// Fail records a failed sign-in and reports whether this failure filled the
// bucket (the moment the IP became blocked).
func (l *authLimiter) Fail(ip string) (justBlocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{at: now}
		l.buckets[ip] = b
	}
	l.drain(b, now)
	before := b.level
	b.level++
	l.maybeSweep(now)
	return before <= l.Burst-1 && b.level > l.Burst-1
}

// maybeSweep forgets empty buckets so memory stays bounded. Callers hold l.mu.
func (l *authLimiter) maybeSweep(now time.Time) {
	if now.Sub(l.sweep) < time.Minute && len(l.buckets) < 100000 {
		return
	}
	l.sweep = now
	for ip, b := range l.buckets {
		l.drain(b, now)
		if b.level == 0 {
			delete(l.buckets, ip)
		}
	}
}

// clientIP returns the caller's address. Behind a platform proxy the real
// client is in a header the proxy sets; otherwise it's the connection's peer.
// The header is only trusted when configured, since anyone can send it.
func (s *Server) clientIP(r *http.Request) string {
	if h := s.opts.ClientIPHeader; h != "" {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			// X-Forwarded-For lists every hop; the proxy appends the client it
			// saw last, and anything to the left of that came from the client.
			if strings.EqualFold(h, "X-Forwarded-For") {
				parts := strings.Split(v, ",")
				v = strings.TrimSpace(parts[len(parts)-1])
			}
			if ip := net.ParseIP(v); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeTooMany(w http.ResponseWriter, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests,
		"too many failed sign-in attempts from your network; try again in "+strconv.Itoa(secs)+" seconds")
}

// checkBlocked answers 429 and returns true if the request's IP is blocked
// and it doesn't carry a credential that has worked before. cred overrides
// the credential found in the request (the JWT exchange carries it in the path).
func (s *Server) checkBlocked(w http.ResponseWriter, r *http.Request, cred ...string) bool {
	if wait := s.limiter.Blocked(s.clientIP(r)); wait > 0 {
		c := auth.PresentedCredential(r)
		if len(cred) > 0 {
			c = cred[0]
		}
		if s.limiter.Known(c) {
			return false
		}
		writeTooMany(w, wait)
		return true
	}
	return false
}

// authFailed records a failed sign-in, auditing the moment an IP is blocked.
func (s *Server) authFailed(r *http.Request) {
	ip := s.clientIP(r)
	if s.limiter.Fail(ip) {
		s.log.Warn("blocking sign-in attempts after repeated failures", "ip", ip)
		if err := s.store.Audit(r.Context(), ip, "sign-in blocked", "too many failed attempts"); err != nil {
			s.log.Warn("audit write failed", "err", err)
		}
	}
}
