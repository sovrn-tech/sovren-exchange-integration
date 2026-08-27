package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

func tempKitWithContacts(t *testing.T, contacts, security string) *RunContext {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "contacts.md"), []byte(contacts), 0o644))
	if security != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SECURITY.md"), []byte(security), 0o644))
	}
	return &RunContext{Mode: "testnet", KitRoot: dir, Log: logging.New("sovren-cert-test")}
}

// publishedContacts builds a placeholder-free docs/contacts.md with a labelled
// Security contact + Status page section (what G1 parses).
func publishedContacts(statusURL, secEmail string) string {
	return "# Contacts\n\n" +
		"## Technical contacts\n\n| Primary | " + secEmail + " | support |\n\n" +
		"## Security contact\n\n| Security reports | " + secEmail + " | disclosure |\n\n" +
		"## Status page\n\n| Network status page | " + statusURL + " | health |\n"
}

// A contacts file still carrying a D5 placeholder is BLOCKED(D5).
func TestG1BlockedWhenPlaceholdersPresent(t *testing.T) {
	contacts := "# Contacts\n\n## Security contact\n\n| Security reports | *pending publication — plan D5* | x |\n"
	rc := tempKitWithContacts(t, contacts, "")
	r := scenarioG1Contacts(context.Background(), rc)
	require.Equal(t, StatusBlocked, r.Status, r.Reason)
	require.Equal(t, "D5", r.Dependency)
}

// The shipped kit's contacts are now published (D5 filled): no placeholders, the
// security contact is present in SECURITY.md, and a status-page URL exists.
// (Asserted directly, without live probes, so the unit test needs no network.)
func TestG1ShippedKitIsPublishedAndConsistent(t *testing.T) {
	root := testKitRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "docs", "contacts.md"))
	require.NoError(t, err)
	require.Equal(t, 0, strings.Count(string(body), d5Placeholder), "shipped contacts.md must be published (no D5 placeholders)")

	secEmail := reEmail.FindString(sectionText(string(body), "Security contact"))
	require.NotEmpty(t, secEmail, "shipped contacts.md must carry a security-reports email")
	sec, err := os.ReadFile(filepath.Join(root, "SECURITY.md"))
	require.NoError(t, err)
	require.Contains(t, strings.ToLower(string(sec)), strings.ToLower(secEmail), "security contact must appear in SECURITY.md")

	require.NotEmpty(t, reURL.FindString(sectionText(string(body), "Status page")), "shipped contacts.md must carry a status-page URL")
}

func TestG1PassesWhenPublishedAndResolvable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rc := tempKitWithContacts(t,
		publishedContacts(srv.URL, "security@sovren.example"),
		"Report vulnerabilities to security@sovren.example (2 business days).\n")
	r := scenarioG1Contacts(context.Background(), rc)
	require.Equal(t, StatusPass, r.Status, r.Reason)
}

// Review finding 1: the security contact must actually appear in SECURITY.md.
func TestG1FailsWhenSecurityContactAbsentFromSecurityMD(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rc := tempKitWithContacts(t,
		publishedContacts(srv.URL, "security@sovren.example"),
		"Report vulnerabilities privately. (no address here)\n")
	r := scenarioG1Contacts(context.Background(), rc)
	require.Equal(t, StatusFail, r.Status)
	require.Contains(t, r.Reason, "SECURITY.md")
}

// Review finding 2: a status-page URL is required, not just any address.
func TestG1FailsWhenNoStatusURL(t *testing.T) {
	contacts := "# Contacts\n\n## Security contact\n\n| Security reports | security@sovren.example | x |\n\n" +
		"## Status page\n\n| Network status page | (not yet) | health |\n"
	rc := tempKitWithContacts(t, contacts, "security@sovren.example\n")
	r := scenarioG1Contacts(context.Background(), rc)
	require.Equal(t, StatusFail, r.Status)
	require.Contains(t, r.Reason, "status page")
}

func TestG1FailsOnUnresolvableURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	rc := tempKitWithContacts(t,
		publishedContacts(srv.URL, "security@sovren.example"),
		"security@sovren.example\n")
	r := scenarioG1Contacts(context.Background(), rc)
	require.Equal(t, StatusFail, r.Status)
	require.Contains(t, r.Reason, "unresolvable")
}

func TestG1SkipsWhenNoConnectivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	url := srv.URL
	srv.Close() // now unreachable — every probe transport-errors
	rc := tempKitWithContacts(t,
		publishedContacts(url, "security@sovren.example"),
		"security@sovren.example\n")
	r := scenarioG1Contacts(context.Background(), rc)
	require.Equal(t, StatusSkipped, r.Status, r.Reason)
}
