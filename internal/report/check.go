package report

import (
	"io"

	"github.com/tallica/incus-beacon/internal/check"
)

type CheckRow struct {
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Status   string `json:"status"`
	Image    string `json:"image"`
	Latest   string `json:"latest,omitempty"`
	Note     string `json:"note,omitempty"`
}

func CheckRows(results []check.Result) []CheckRow {
	rows := make([]CheckRow, 0, len(results))
	for i := range results {
		r := &results[i]
		rows = append(rows, CheckRow{
			Project:  r.Target.Project,
			Instance: r.Target.Instance,
			Status:   string(r.Status),
			Image:    r.Current,
			Latest:   r.Latest,
			Note:     r.Reason,
		})
	}
	return rows
}

func WriteCheck(w io.Writer, format string, rows []CheckRow) error {
	if format == "json" {
		return writeJSON(w, rows)
	}
	table := [][]string{{"PROJECT", "INSTANCE", "STATUS", "IMAGE", "LATEST", "NOTE"}}
	for _, r := range rows {
		table = append(table, []string{r.Project, r.Instance, r.Status, dash(r.Image), dash(r.Latest), r.Note})
	}
	return writeTable(w, table)
}
