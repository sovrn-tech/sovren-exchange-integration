package main

// Group G — go-live & operational readiness. Release-time checks on the
// operational service points the kit publishes (as opposed to the on-chain /
// adapter behaviour the other groups drive). G1 gates FR-053: the contacts &
// status-page doc must be published and resolvable before GA.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func init() {
	register("G1", scenarioG1Contacts)
}

// d5Placeholder is the marker every unpublished FR-053 value carries in
// docs/contacts.md (`*pending publication — plan D5*`). Its presence means D5 is
// still open; the sanitizer also asserts none survive into a GA export.
const d5Placeholder = "pending publication"

var (
	reURL   = regexp.MustCompile(`https?://[^\s)>\]]+`)
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
)

// scenarioG1Contacts verifies FR-053: the exchange-facing contacts & service
// points (docs/contacts.md) are published and resolvable.
//
//   - While the D5 placeholders remain → BLOCKED(D5) (does not fail a run, but a
//     GA certification requires zero BLOCKED).
//   - Once published, the specific labelled service points must be present and
//     consistent: the "## Security contact" address must ALSO appear in
//     SECURITY.md, the "## Status page" must carry a URL, and every URL in the
//     doc must resolve (HTTP < 400). Mail *acceptance* (SMTP) is a manual
//     release-owner check; here we assert address form + cross-document match.
func scenarioG1Contacts(ctx context.Context, rc *RunContext) Result {
	path := filepath.Join(rc.KitRoot, "docs", "contacts.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return fail("cannot read docs/contacts.md: "+err.Error(), nil)
	}
	body := string(data)

	if n := strings.Count(body, d5Placeholder); n > 0 {
		return Result{
			Status:     StatusBlocked,
			Dependency: "D5",
			Reason:     fmt.Sprintf("%d FR-053 service point(s) unpublished (%q); publish real values to close D5", n, d5Placeholder),
			Evidence:   map[string]any{"placeholders": n, "doc": "docs/contacts.md"},
		}
	}

	// Identify the specific labelled service points (not just "any" address/URL).
	secEmail := reEmail.FindString(sectionText(body, "Security contact"))
	statusURL := reURL.FindString(sectionText(body, "Status page"))

	var problems []string
	if secEmail == "" {
		problems = append(problems, "no security-reports email under '## Security contact'")
	} else if sec, err := os.ReadFile(filepath.Join(rc.KitRoot, "SECURITY.md")); err != nil {
		problems = append(problems, "cannot read SECURITY.md to cross-check the security contact: "+err.Error())
	} else if !strings.Contains(strings.ToLower(string(sec)), strings.ToLower(secEmail)) {
		problems = append(problems, fmt.Sprintf("security contact %q does not appear in SECURITY.md", secEmail))
	}
	if statusURL == "" {
		problems = append(problems, "no network status page URL under '## Status page'")
	}
	if len(problems) > 0 {
		return fail("FR-053 doc incomplete: "+strings.Join(problems, "; "),
			map[string]any{"security_contact": secEmail, "status_page": statusURL})
	}

	// Probe every URL (status page + channels). At least the status page exists.
	urls := uniqStrings(reURL.FindAllString(body, -1))
	client := &http.Client{Timeout: 8 * time.Second}
	var broken []string
	reached := 0
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			broken = append(broken, fmt.Sprintf("%s (bad url: %v)", u, err))
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			broken = append(broken, fmt.Sprintf("%s (%v)", u, err))
			continue
		}
		reached++
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			broken = append(broken, fmt.Sprintf("%s (HTTP %d)", u, resp.StatusCode))
		}
	}
	// Nothing responded at all: the environment has no connectivity to verify
	// resolvability (e.g. offline CI). SKIP rather than FAIL — the GA gate is the
	// testnet run, which has network.
	if reached == 0 {
		return skip("no FR-053 URL was reachable (offline?); re-run with network access to verify resolvability")
	}
	if len(broken) > 0 {
		return fail("unresolvable FR-053 endpoint(s): "+strings.Join(broken, "; "),
			map[string]any{"urls": urls, "broken": broken})
	}
	return pass(map[string]any{"urls": urls, "security_contact": secEmail, "status_page": statusURL, "resolvable": true})
}

// sectionText returns the lines under a `## <heading>` markdown section, up to
// the next `## ` heading (or end of document).
func sectionText(body, heading string) string {
	var out []string
	in := false
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "## ") {
			in = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(t, "## ")), heading)
			continue
		}
		if in {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
