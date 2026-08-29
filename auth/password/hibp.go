package password

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HIBP checks a password against Have I Been Pwned's range API.
//
// The password never leaves the process. Only the first five hex digits of its
// SHA-1 are sent; the service answers with every suffix it holds under that
// prefix — several hundred of them — and the match happens here. That is what
// makes asking a third party about a password defensible: the third party
// learns that somebody, somewhere, has a password whose hash starts with those
// five characters, which is true of millions of people.
//
// SHA-1 is not a choice; it is the format the range API speaks. It is being
// used as a lookup key against a public corpus, not to protect anything.
type HIBP struct {
	// Client defaults to one with a short timeout. A password check is on the
	// sign-up path, and a hung request there is worse than a skipped check.
	Client *http.Client
	// BaseURL defaults to the public API. It is settable so a test can serve
	// its own answers and an air-gapped deployment can host a mirror.
	BaseURL string
}

// NewHIBP builds a checker against the public API.
func NewHIBP() *HIBP {
	return &HIBP{
		Client:  &http.Client{Timeout: 3 * time.Second},
		BaseURL: "https://api.pwnedpasswords.com",
	}
}

// Count implements [BreachChecker].
func (h *HIBP) Count(ctx context.Context, plain string) (int, error) {
	sum := sha1.Sum([]byte(plain))
	digest := strings.ToUpper(hex.EncodeToString(sum[:]))
	prefix, suffix := digest[:5], digest[5:]

	base := h.BaseURL
	if base == "" {
		base = "https://api.pwnedpasswords.com"
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/range/"+prefix, nil)
	if err != nil {
		return 0, err
	}
	// Padding asks the service to return a variable number of decoy entries,
	// so the response size does not narrow down which prefix was requested for
	// anyone watching the connection.
	req.Header.Set("Add-Padding", "true")

	res, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("password: breach check: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("password: breach check: unexpected status %d", res.StatusCode)
	}

	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		line := scanner.Text()
		rest, ok := strings.CutPrefix(line, suffix+":")
		if !ok {
			continue
		}
		// A padded entry has a count of zero, which is also the honest answer
		// for a hash the corpus does not contain.
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return 0, fmt.Errorf("password: breach check: unreadable count %q", rest)
		}
		return n, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("password: breach check: %w", err)
	}
	return 0, nil
}
