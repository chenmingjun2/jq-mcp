package app

import (
	"regexp"
	"strings"
)

// standardBegin/standardEnd bound the injected block so it can be replaced on
// repeat runs instead of piling up.
const (
	standardBegin = "# jq-mcp-standard-begin"
	standardEnd   = "# jq-mcp-standard-end"
)

// standardBlock is injected at the top of initialize(context). It normalizes
// slippage and trading cost so backtests are comparable across strategies.
const standardBlock = `    ` + standardBegin + `
    # 统一滑点与交易成本（由 jq-mcp 注入，可重复执行不叠加）
    set_slippage(FixedSlippage(0.002), type="fund")
    set_slippage(FixedSlippage(0.02), type="stock")
    set_order_cost(
        OrderCost(
            open_tax=0,
            close_tax=0.001,
            open_commission=0.0003,
            close_commission=0.0003,
            close_today_commission=0,
            min_commission=5,
        ),
        type="stock",
    )
    set_order_cost(
        OrderCost(
            open_tax=0,
            close_tax=0,
            open_commission=0,
            close_commission=0,
            close_today_commission=0,
            min_commission=0,
        ),
        type="mmf",
    )
    ` + standardEnd

var (
	markerRe    = regexp.MustCompile(`(?ms)^[ \t]*# jq-mcp-standard-begin.*?^[ \t]*# jq-mcp-standard-end[^\n]*`)
	initLineRe  = regexp.MustCompile(`^[ \t]*def[ \t]+initialize[ \t]*\(`)
	minimalTmpl = "def initialize(context):\n    pass\n\n\ndef handle_data(context, data):\n    pass\n"
)

// StandardizeCode injects the standard slippage/cost block into the strategy
// source. It replaces an existing marker block when present, otherwise inserts
// after the initialize(context) line, otherwise prepends a new initialize.
func StandardizeCode(code string) string {
	code = strings.ReplaceAll(code, "\r\n", "\n")
	if strings.Contains(code, standardBegin) && strings.Contains(code, standardEnd) {
		return markerRe.ReplaceAllString(code, standardBlock)
	}
	lines := strings.Split(code, "\n")
	for i, line := range lines {
		if initLineRe.MatchString(line) {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			block := standardBlock
			if indent != "" {
				block = indent + strings.TrimPrefix(standardBlock, "    ")
			}
			out := make([]string, 0, len(lines)+len(strings.Split(block, "\n")))
			out = append(out, lines[:i+1]...)
			out = append(out, strings.Split(block, "\n")...)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n")
		}
	}
	return "def initialize(context):\n" + standardBlock + "\n" + code
}

// MinimalTemplate returns a minimal runnable JoinQuant strategy template.
func MinimalTemplate() string { return minimalTmpl }
