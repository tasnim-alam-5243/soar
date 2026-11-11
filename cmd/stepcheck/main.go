// Command stepcheck runs the live checks of build Steps 5, 6 and 7 against a running
// stack (`make up`) and prints one PASS / FAIL / SKIP line per check, with a hint for
// every failure.
//
//	make check-steps                   # Steps 5, 6 and 7 (about 3 minutes)
//	go run ./cmd/stepcheck -step 6     # one step
//
// It only talks to the public HTTP APIs (api on :8080, ingest on :8081). Each run adds a
// few alerts, workflows and executions whose names start with "stepcheck" to the dev
// data. The one shared setting it edits (the wazuh_noisy_rule_ids list) is put back, and
// the secret and connection it creates are deleted.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// check is one live test. Run returns a short detail for the PASS line, or an error.
type check struct {
	ID    string
	Title string
	Hint  string // where to look when it fails
	Run   func() (string, error)
}

type step struct {
	N      int
	Title  string
	Checks []check
	After  func() // cleanup, runs even when checks fail
}

// skipError marks a check that cannot run here (for example: no Wazuh indexer configured).
type skipError struct{ msg string }

func (e skipError) Error() string { return e.msg }

func skip(format string, a ...any) error { return skipError{fmt.Sprintf(format, a...)} }

func main() {
	env := loadDotEnv("deploy/compose/.env")
	steps := flag.String("step", "5,6,7", "comma-separated steps to check (5, 6, 7)")
	api := flag.String("api", setting(env, "http://127.0.0.1:8080", "SOAR_API_URL"), "api base URL")
	ingest := flag.String("ingest", setting(env, "http://127.0.0.1:8081", "SOAR_INGEST_URL"), "ingest base URL")
	key := flag.String("key", setting(env, "soar_dev-bootstrap-key-change-me-0123456789abcdef", "SOAR_API_KEY", "SOAR_BOOTSTRAP_API_KEY"), "admin API key")
	token := flag.String("token", setting(env, "dev-wazuh-token-change-me", "SOAR_SOURCE_TOKEN_WAZUH_PROD"), "wazuh-prod ingest token")
	flag.String("os", setting(env, "http://127.0.0.1:9200", "SOAR_OS_URL"), "dev OpenSearch URL (fake Wazuh indexer of check 5.12)")
	sync := flag.Bool("sync-pull-source", false, "copy the current wazuh-pull pack config onto the existing source, then exit")
	flag.Parse()

	osURL := flag.Lookup("os").Value.String()
	c := &client{
		apiURL: strings.TrimRight(*api, "/"), ingestURL: strings.TrimRight(*ingest, "/"), osURL: strings.TrimRight(osURL, "/"), key: *key, token: *token,
		hc: &http.Client{Timeout: 60 * time.Second}, run: fmt.Sprint(time.Now().Unix()),
	}
	if *sync {
		if err := syncPullSource(c); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	p := newPrinter(os.Stdout)
	p.header(c)

	all := map[string]func(*client) step{"5": step5, "6": step6, "7": step7}
	var passed, failed, skipped int
	for _, s := range strings.Split(*steps, ",") {
		mk, ok := all[strings.TrimSpace(s)]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown step %q (use 5, 6 or 7)\n", s)
			os.Exit(2)
		}
		st := mk(c)
		p.step(st)
		var sp, sf, ss int
		for _, ch := range st.Checks {
			p.start(ch)
			t0 := time.Now()
			detail, err := ch.Run()
			var sk skipError
			switch {
			case err == nil:
				sp++
				p.result("PASS", ch, detail, time.Since(t0), "")
			case errors.As(err, &sk):
				ss++
				p.result("SKIP", ch, sk.msg, time.Since(t0), "")
			default:
				sf++
				p.result("FAIL", ch, err.Error(), time.Since(t0), ch.Hint)
			}
		}
		if st.After != nil {
			st.After()
		}
		p.stepSummary(st, sp, sf, ss)
		passed, failed, skipped = passed+sp, failed+sf, skipped+ss
	}
	p.summary(passed, failed, skipped)
	if failed > 0 {
		os.Exit(1)
	}
}

// setting returns the first non-empty environment variable, then the same name in
// deploy/compose/.env, then def.
func setting(dotenv map[string]string, def string, names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	for _, n := range names {
		if v := dotenv[n]; v != "" {
			return v
		}
	}
	return def
}

// loadDotEnv reads KEY=VALUE lines (comments and blank lines ignored, quotes stripped).
func loadDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.TrimSpace(v)
		if i := strings.Index(v, " #"); i >= 0 && !strings.HasPrefix(v, `"`) && !strings.HasPrefix(v, "'") {
			v = strings.TrimSpace(v[:i])
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// ---- output ----

type printer struct {
	color bool
	out   *os.File
}

func newPrinter(f *os.File) *printer {
	fi, err := f.Stat()
	return &printer{out: f, color: err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""}
}

func (p *printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (p *printer) header(c *client) {
	fmt.Fprintf(p.out, "SOAR step check  (api %s, ingest %s, run id %s)\n", c.apiURL, c.ingestURL, c.run)
}

func (p *printer) step(s step) {
	fmt.Fprintf(p.out, "\n%s\n", p.paint("1", fmt.Sprintf("Step %d - %s", s.N, s.Title)))
}

func (p *printer) start(ch check) {
	if p.color {
		fmt.Fprintf(p.out, "  ....  %-5s %s", ch.ID, ch.Title)
	}
}

func (p *printer) result(word string, ch check, detail string, took time.Duration, hint string) {
	if p.color {
		fmt.Fprint(p.out, "\r\033[K")
	}
	colors := map[string]string{"PASS": "32", "FAIL": "31;1", "SKIP": "33"}
	fmt.Fprintf(p.out, "  %s  %-5s %s %s\n", p.paint(colors[word], word), ch.ID, ch.Title, p.paint("2", fmt.Sprintf("(%.1fs)", took.Seconds())))
	if detail != "" {
		label := "      "
		if word == "FAIL" {
			label = "      problem: "
		}
		fmt.Fprintf(p.out, "%s%s\n", label, detail)
	}
	if hint != "" {
		fmt.Fprintf(p.out, "      look at: %s\n", hint)
	}
}

func (p *printer) stepSummary(s step, pass, fail, sk int) {
	line := fmt.Sprintf("Step %d: %d passed, %d failed, %d skipped", s.N, pass, fail, sk)
	if fail > 0 {
		line = p.paint("31;1", line)
	} else {
		line = p.paint("32", line)
	}
	fmt.Fprintln(p.out, "  "+line)
}

func (p *printer) summary(pass, fail, sk int) {
	verdict := p.paint("32;1", "ALL CHECKS PASSED")
	if fail > 0 {
		verdict = p.paint("31;1", fmt.Sprintf("%d CHECK(S) FAILED", fail))
	}
	fmt.Fprintf(p.out, "\n%s  (%d passed, %d failed, %d skipped)\n", verdict, pass, fail, sk)
}
