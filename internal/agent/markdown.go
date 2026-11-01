package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Markdown renders the report for humans: terminal, file or web page.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# podmortem: %s/%s\n\n", r.Namespace, r.Pod)
	fmt.Fprintf(&b, "_Trigger: %s · model: %s · took %s · id %s_\n\n", orDash(r.Trigger), r.Model, r.FinishedAt.Sub(r.StartedAt).Round(100*time.Millisecond), r.ID)
	if r.Error != "" {
		fmt.Fprintf(&b, "> **Investigation failed:** %s\n\n", r.Error)
	}
	if len(r.Signals) > 0 {
		b.WriteString("**Detected signals**\n")
		for _, s := range r.Signals {
			b.WriteString("- " + s + "\n")
		}
		b.WriteString("\n")
	}
	if r.Analysis != "" {
		b.WriteString(r.Analysis + "\n\n")
	}
	if len(r.Steps) > 0 {
		b.WriteString("<details><summary>Tool calls made by the agent</summary>\n\n")
		for i, s := range r.Steps {
			args, _ := json.Marshal(s.Args)
			fmt.Fprintf(&b, "%d. `%s %s`\n\n```\n%s\n```\n", i+1, s.Tool, args, strings.TrimSpace(s.Output))
		}
		b.WriteString("</details>\n\n")
	}
	if r.Evidence != "" {
		b.WriteString("<details><summary>Evidence collected</summary>\n\n```\n" + strings.TrimSpace(r.Evidence) + "\n```\n</details>\n")
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "manual"
	}
	return s
}
