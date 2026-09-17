package core

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"
)

// WriteHistoryCSV writes the ledger as a spreadsheet: one row per entry,
// newest first, then a blank line and the totals. Amounts are whole sats,
// signed the way the screen shows them; a move between the app's own
// balances has no sign.
func WriteHistoryCSV(w io.Writer, h *History) error {
	c := csv.NewWriter(w)
	if err := c.Write([]string{"date", "what", "details", "amount_sat", "fee_sat", "fee_taken_by", "status", "transaction", "address"}); err != nil {
		return err
	}
	for _, e := range h.Entries {
		when := ""
		if e.Time > 0 {
			when = time.Unix(e.Time, 0).Local().Format("2006-01-02 15:04")
		}
		amount := strconv.FormatInt(e.Amount, 10)
		switch {
		case e.Delta > 0:
			amount = "+" + amount
		case e.Delta < 0:
			amount = "-" + amount
		}
		fee := ""
		if e.Fee != 0 {
			fee = strconv.FormatInt(e.Fee, 10)
		}
		note := e.Detail
		if e.Note != "" {
			note += " " + e.Note
		}
		if err := c.Write([]string{when, e.Title, note, amount, fee, e.FeeNote, e.Status, e.TxID, e.Address}); err != nil {
			return err
		}
	}
	t := h.Totals
	rows := [][]string{
		{},
		{"", "Received", "", "+" + strconv.FormatInt(t.In, 10)},
		{"", "Sent", "", "-" + strconv.FormatInt(t.Out, 10)},
		{"", "Fees", "", "-" + strconv.FormatInt(t.Fees, 10)},
		{"", "Total of this list", "", strconv.FormatInt(t.Expected, 10)},
		{"", "In this app now", "", strconv.FormatInt(t.Held, 10)},
	}
	for _, r := range rows {
		if err := c.Write(r); err != nil {
			return err
		}
	}
	c.Flush()
	return c.Error()
}
