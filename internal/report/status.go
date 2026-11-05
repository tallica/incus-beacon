// Package report renders results for people (a table) and scripts (JSON).
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
)

// StatusUnchecked is shown for instances incus-beacon has not recorded yet.
const StatusUnchecked = "unchecked"

type StatusRow struct {
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Status   string `json:"status"`
	Image    string `json:"image"`
	Latest   string `json:"latest,omitempty"`
	Since    string `json:"since,omitempty"`
	Note     string `json:"note,omitempty"`
}

func StatusRows(targets []incus.Target, onlyUpdates bool) []StatusRow {
	rows := []StatusRow{}
	for i := range targets {
		t := &targets[i]
		if onlyUpdates && t.Beacon.Status != "update-available" {
			continue
		}
		r := StatusRow{
			Project:  t.Project,
			Instance: t.Instance,
			Status:   t.Beacon.Status,
			Image:    t.Beacon.Current,
			Latest:   t.Beacon.Latest,
			Since:    t.Beacon.Since,
			Note:     t.Beacon.Error,
		}
		if r.Status == "" {
			r.Status = StatusUnchecked
		}
		pre := check.Precheck(*t)
		if r.Image == "" {
			r.Image = pre.Current
		}
		if t.Beacon.Status == "" || t.PolicyErr != nil {
			r.Note = pre.Reason
		}
		rows = append(rows, r)
	}
	return rows
}

func WriteStatus(w io.Writer, format string, rows []StatusRow) error {
	if format == "json" {
		return writeJSON(w, rows)
	}
	table := [][]string{{"PROJECT", "INSTANCE", "STATUS", "IMAGE", "LATEST", "SINCE", "NOTE"}}
	for _, r := range rows {
		table = append(table, []string{r.Project, r.Instance, r.Status, dash(r.Image), dash(r.Latest), dash(r.Since), r.Note})
	}
	return writeTable(w, table)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeTable(w io.Writer, rows [][]string) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return writeTrimmed(w, buf.Bytes())
}

// writeTrimmed drops the padding tabwriter leaves before an empty last column.
func writeTrimmed(w io.Writer, table []byte) error {
	for line := range bytes.Lines(table) {
		if _, err := fmt.Fprintf(w, "%s\n", bytes.TrimRight(line, " \n")); err != nil {
			return err
		}
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
