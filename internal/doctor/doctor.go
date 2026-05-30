package doctor

import (
	"fmt"
	"io"
	"strings"

	"github.com/c/just-talk-go/config"
)

type Severity int

const (
	Required Severity = iota
	Warning
)

type Check struct {
	Name     string
	OK       bool
	Severity Severity
	Detail   string
	Fix      string
}

type Report struct {
	Platform string
	Backend  string
	Checks   []Check
}

func Run(cfg *config.Config, backend string) Report {
	return runPlatform(cfg, backend)
}

func (r Report) Healthy() bool {
	for _, check := range r.Checks {
		if check.Severity == Required && !check.OK {
			return false
		}
	}
	return true
}

func (r Report) Print(w io.Writer) {
	fmt.Fprintln(w, "Just Talk Doctor")
	fmt.Fprintf(w, "平台: %s", fallback(r.Platform, "unknown"))
	if r.Backend != "" {
		fmt.Fprintf(w, " / %s", r.Backend)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	for _, check := range r.Checks {
		mark := "✓"
		if !check.OK {
			if check.Severity == Warning {
				mark = "!"
			} else {
				mark = "✗"
			}
		}
		fmt.Fprintf(w, "%s %s", mark, check.Name)
		if check.Detail != "" {
			fmt.Fprintf(w, ": %s", check.Detail)
		}
		fmt.Fprintln(w)
		if !check.OK && check.Fix != "" {
			fmt.Fprintf(w, "  修复: %s\n", check.Fix)
		}
	}

	if r.Healthy() {
		fmt.Fprintln(w, "\n结果: healthy")
	} else {
		fmt.Fprintln(w, "\n结果: unhealthy，已停止启动。修复上面的项目后再运行 just-talk。")
	}
}

func fallback(s, v string) string {
	if strings.TrimSpace(s) == "" {
		return v
	}
	return s
}
