package results

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// words splits an identifier or header into lower-case words at separators
// and at camelCase boundaries: "decodeFastTokensPerSecond" gives decode fast
// tokens per second, "ttft_ms" gives ttft ms, "p99" stays one word.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// tokens are the lower-case alphanumeric runs of a text, for matching a
// saved fact against a row's setup. "8B" and "8b" are one token.
func tokens(s string) []string {
	var out []string
	var cur []rune
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, unicode.ToLower(r))
			continue
		}
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// metricWords are the words that make a name a measured quantity: speed,
// latency, quality scores, ratios and cost. A name is a metric when any of
// its words is here. The list is deliberately narrow: a number under another
// name (a count, a seed, a version, a size) is not a result.
var metricWords = map[string]bool{
	"tps": true, "toks": true, "qps": true, "rps": true, "throughput": true,
	"latency": true, "ttft": true, "tpot": true, "itl": true, "p50": true, "p90": true, "p95": true, "p99": true, "p999": true,
	"accept": true, "acceptance": true, "accuracy": true, "acc": true, "f1": true, "ndcg": true, "mrr": true, "recall": true, "precision": true,
	"score": true, "auc": true, "auroc": true, "ppl": true, "perplexity": true, "bleu": true, "rouge": true, "wer": true, "cer": true,
	"speedup": true, "tflops": true, "gflops": true, "mfu": true, "bandwidth": true, "gbps": true, "mbps": true,
	"hitrate": true, "winrate": true, "passrate": true, "loss": true, "ratio": true,
	"ms": true, "msec": true, "millis": true, "milliseconds": true, "seconds": true, "secs": true,
}

// notMetricWords veto a metric word: "accept_n" counts accepted tokens, it is
// not the acceptance rate.
var notMetricWords = map[string]bool{
	"n": true, "d": true, "count": true, "counts": true, "num": true, "number": true, "total": true, "id": true, "idx": true,
	"index": true, "seed": true, "steps": true, "step": true, "rows": true, "len": true, "length": true, "bytes": true, "hash": true, "sha": true,
	"version": true,
	// settings that happen to end in _ms or _s: a deadline is configured, not measured
	"timeout": true, "deadline": true, "budget": true, "interval": true, "period": true, "window": true, "estimate": true, "estimated": true,
	"expected": true, "threshold": true, "limit": true, "target": true, "cap": true, "default": true, "delay": true, "retry": true,
	"ttl": true, "lease": true, "quota": true, "sleep": true, "wait": true,
}

// isMetric reports whether a name denotes a measured quantity and gives the
// unit its words imply ("" when they imply none).
func isMetric(name string) (bool, string) {
	ws := words(name)
	if len(ws) == 0 {
		return false, ""
	}
	for _, w := range ws {
		if notMetricWords[w] {
			return false, ""
		}
	}
	joined := " " + strings.Join(ws, " ") + " "
	perSec := strings.Contains(joined, " per second ") || strings.Contains(joined, " per sec ") ||
		strings.Contains(joined, " tok s ") || strings.Contains(joined, " tokens s ") || strings.Contains(joined, " tokens sec ")
	hit := perSec
	unit := ""
	if perSec {
		unit = "tok/s"
	}
	for i, w := range ws {
		if metricWords[w] {
			hit = true
			switch w {
			case "tps", "toks":
				unit = "tok/s"
			case "qps":
				unit = "qps"
			case "rps":
				unit = "req/s"
			case "ms", "msec", "millis", "milliseconds":
				unit = "ms"
			case "seconds", "secs":
				unit = "s"
			case "speedup":
				unit = "x"
			case "tflops":
				unit = "TFLOPS"
			case "gbps":
				unit = "GB/s"
			case "mbps":
				unit = "MB/s"
			}
		}
		// a trailing "_s" or "_pct" is a unit on something measured
		if i == len(ws)-1 && i > 0 {
			switch w {
			case "s", "sec":
				hit = true
				if unit == "" {
					unit = "s"
				}
			case "pct", "percent":
				hit = true
				unit = "%"
			}
		}
	}
	if perSec && unit == "" {
		unit = "tok/s"
	}
	return hit, unit
}

// medianWords mark how a number was summarised in its header.
func basisOf(name string) string {
	for _, w := range words(name) {
		switch w {
		case "median", "med", "p50":
			return "median"
		case "mean", "avg", "average":
			return "mean"
		case "best", "max", "min", "peak":
			return w
		}
	}
	return ""
}

// configNumeric are numeric columns that describe the setup of a run rather
// than its outcome.
var configNumeric = map[string]bool{
	"batch": true, "bs": true, "c": true, "conc": true, "concurrency": true, "k": true, "ctx": true, "context": true,
	"seq": true, "isl": true, "osl": true, "threads": true, "gpus": true, "tp": true, "ep": true, "pp": true, "depth": true,
	"temp": true, "temperature": true, "ngen": true, "nctx": true, "rate": true, "clients": true, "workers": true,
}

func isConfigNumeric(name string) bool {
	ws := words(name)
	for _, w := range ws {
		if configNumeric[w] {
			return true
		}
	}
	return false
}

// unitAlias maps the units found in text to one spelling.
var unitAlias = map[string]string{
	"tok/s": "tok/s", "tokens/s": "tok/s", "tokens/sec": "tok/s", "tok/sec": "tok/s", "t/s": "tok/s", "tps": "tok/s",
	"r/s": "req/s", "req/s": "req/s", "rps": "req/s", "qps": "qps", "ops/s": "ops/s",
	"ms": "ms", "us": "us", "µs": "us", "ns": "ns", "s": "s", "sec": "s",
	"%": "%", "x": "x", "×": "x", "gb/s": "GB/s", "mb/s": "MB/s", "tflops": "TFLOPS", "gb": "GB", "mb": "MB",
	"pt": "pt", "pts": "pt", "pp": "pt",
}

func canonUnit(u string) string {
	u = strings.TrimSpace(u)
	if a, ok := unitAlias[strings.ToLower(u)]; ok {
		return a
	}
	return ""
}

var cellRe = regexp.MustCompile(`^[~≈]?\$?(-?\d{1,3}(?:,\d{3})+(?:\.\d+)?|-?\d+(?:\.\d+)?)\s?(%|x|×|ms|µs|us|ns|s|sec|tok/s|tokens/s|tokens/sec|tok/sec|t/s|tps|r/s|req/s|rps|qps|ops/s|GB/s|MB/s|TFLOPS|GB|MB|pts?|pp)?$`)

// parseCell reads a table cell that holds exactly one number: "52.7",
// "7,412 r/s", "**1.92x**", "~7 ms". Ranges, arrows and prose are not
// numbers here.
func parseCell(s string) (v float64, dec int, unit string, ok bool) {
	v, dec, unit, _, ok = parseCellRaw(s)
	return
}

// parseCellRaw is parseCell that also returns the number as written, without
// its unit or thousands commas.
func parseCellRaw(s string) (v float64, dec int, unit, raw string, ok bool) {
	s = strings.TrimSpace(strings.Trim(s, "*_` "))
	m := cellRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, "", "", false
	}
	v, dec, ok = parseNumber(m[1])
	if !ok {
		return 0, 0, "", "", false
	}
	return v, dec, canonUnit(m[2]), strings.ReplaceAll(m[1], ",", ""), true
}

// parseNumber reads digits with optional thousands commas; dec is how many
// decimals the text wrote.
func parseNumber(s string) (float64, int, bool) {
	s = strings.ReplaceAll(s, ",", "")
	dec := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		dec = len(s) - i - 1
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, 0, false
	}
	return f, dec, true
}

// roundTo rounds v to dec decimals.
func roundTo(v float64, dec int) float64 {
	p := math.Pow(10, float64(dec))
	return math.Round(v*p) / p
}

// sameAs reports whether a number written with dec decimals names v.
func sameAs(written float64, dec int, v float64) bool {
	return roundTo(v, dec) == roundTo(written, dec)
}

// sigDigits counts the significant digits a literal was written with.
func sigDigits(raw string) int {
	// A whole number's trailing zeros say how it was rounded, not how exact it
	// is: 200, 480 and 1,000 are round figures with one or two digits.
	if !strings.Contains(raw, ".") {
		raw = strings.TrimRight(strings.ReplaceAll(raw, ",", ""), "0")
	}
	n := 0
	started := false
	for _, r := range raw {
		if r == '.' || r == ',' || r == '-' {
			continue
		}
		if r == '0' && !started {
			continue
		}
		if r >= '0' && r <= '9' {
			started = true
			n++
		}
	}
	return n
}
