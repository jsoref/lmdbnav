package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/PowerDNS/lmdb-go/lmdb"
	"github.com/PowerDNS/lmdb-go/lmdbscan"
	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

var (
	app   *tview.Application
	pages *tview.Pages
	env   *lmdb.Env
)

const (
	// MaxDBs is the max number of dbs in the LMDB env.
	// Cost: "7-120 words per transaction" per db, so worst case 0.5 kB per
	// db, or 0.5 MB for 1024.
	MaxDBs      = 1024
	RootDBIName = "<root>"
)

func main() {
	// Get connect string from the command line.
	if len(os.Args) < 2 {
		log.Fatalf("USAGE: %s <lmdb-path>", os.Args[0])
		return
	}

	if err := run(os.Args[1]); err != nil {
		log.Fatalf("Error: %s", err)
	}
}

func run(lmdbPath string) error {
	app = tview.NewApplication()

	fileStat, err := os.Stat(lmdbPath)
	if err != nil {
		return err
	}
	var envFlags uint = lmdb.Readonly
	lmdbFilePath := lmdbPath
	if !fileStat.IsDir() {
		dir, fname := filepath.Split(lmdbPath)
		if fname == "data.mdb" {
			lmdbPath = dir
		} else {
			envFlags |= lmdb.NoSubdir
		}
	} else {
		lmdbFilePath = filepath.Join(lmdbPath, "data.mdb")
	}

	fileStat, err = os.Stat(lmdbFilePath)
	if err != nil {
		return err
	}

	env, err = lmdb.NewEnv()
	if err != nil {
		return err
	}

	err = env.SetMapSize(0) // determine automatically
	if err != nil {
		return err
	}

	err = env.SetMaxDBs(MaxDBs)
	if err != nil {
		return err
	}

	err = env.Open(lmdbPath, envFlags, 0666)
	if err != nil {
		return err
	}
	defer closeWithLog(env)

	info, err := env.Info()
	if err != nil {
		return err
	}
	_ = info

	pages = tview.NewPages()
	footer := tview.NewTextView()
	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(pages, 0, 1, true).
		AddItem(footer, 1, 0, false)
	app.SetRoot(flex, true)

	// Get total space used by all DBIs
	var totalUsed uint64
	err = env.View(func(txn *lmdb.Txn) error {
		rootDBI, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		scanner := lmdbscan.New(txn, rootDBI)
		defer scanner.Close()
		for scanner.Scan() {
			name := string(scanner.Key())
			dbi, err := txn.OpenDBI(name, 0)

			st, err := txn.Stat(dbi)
			if err != nil {
				return err
			}
			totalUsed += sizeBytes(st)
		}
		return scanner.Err()

	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(footer, "MapSize %s", humanize.Bytes(uint64(info.MapSize)))
	_, _ = fmt.Fprintf(footer, " | ")
	_, _ = fmt.Fprintf(footer, "Used %s / %.1f %%",
		humanize.Bytes(totalUsed),
		100.0*float64(totalUsed)/float64(info.MapSize),
	)
	_, _ = fmt.Fprintf(footer, " | ")
	_, _ = fmt.Fprintf(footer, "FileSize %s", humanize.Bytes(uint64(fileStat.Size())))
	_, _ = fmt.Fprintf(footer, " | ")
	_, _ = fmt.Fprintf(footer, "LastTxnID %s", humanize.Comma(info.LastTxnID))

	if err := databasesView(); err != nil {
		return err
	}

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlL:
			app.Sync()
			return nil
		}
		switch event.Rune() {
		case 'Q', 'q':
			app.Stop()
			return nil
		}
		return event
	})

	return app.Run()
}

func dbiView(name string) {
	pageName := "dbi:" + name
	if pages.HasPage(pageName) {
		pages.SwitchToPage(pageName)
		return
	}

	// Setup list of databases view
	table := tview.NewTable()
	table.SetBorder(true).SetTitle(" DBI: " + name + " ")
	table.SetSelectable(true, false)
	table.SetSelectedStyle(tcell.StyleDefault.
		Background(tcell.ColorWhite).Foreground(tcell.ColorBlack))
	table.SetBorderPadding(0, 0, 1, 1)
	// Setup pages
	pages.AddPage(pageName, table, true, true)

	var nextKey, nextVal []byte
	var forwardKey, forwardVal []byte
	var hasBinaryKeys, hasBinaryVals bool

	updateTable := func(back bool) {
		_, _, _, h := pages.GetInnerRect()
		maxRows := h - 1
		wasFirst := false

		var rows KVList
		hasNextPage := false
		err := env.View(func(txn *lmdb.Txn) error {
			var dbi lmdb.DBI
			var err error
			if name == RootDBIName {
				dbi, err = txn.OpenRoot(0)
			} else {
				dbi, err = txn.OpenDBI(name, 0)
			}
			if err != nil {
				return err
			}

			scanner := lmdbscan.New(txn, dbi)
			defer scanner.Close()

			if len(nextVal) > 0 {
				scanner.Set(nextKey, nextVal, lmdb.SetRange)
			} else {
				scanner.Set(nil, nil, lmdb.First)
			}

			if back {
				var next uint = lmdb.Next
				scanner.SetNext(nextKey, nextVal, lmdb.Prev, lmdb.Prev)
				for i := 0; i < maxRows+1; i++ {
					if !scanner.Scan() {
						next = lmdb.First
						wasFirst = true
						break
					}
				}
				scanner.SetNext(nextKey, nextVal, next, lmdb.Next)
				nextKey = scanner.Key()
				nextVal = scanner.Val()
			}

			row := 1 // 0 is the header
			for scanner.Scan() {
				rows = append(rows, KV{
					Key: scanner.Key(),
					Val: scanner.Val(),
				})

				row++
				if row >= (maxRows - 1) {
					if scanner.Scan() {
						forwardKey = scanner.Key()
						forwardVal = scanner.Val()
					}
					hasNextPage = true
					break
				}
			}
			return scanner.Err()
		})
		if err != nil {
			log.Printf("ERROR: %v", err)
		}

		// If we see binary on any page, remember it for this DBI
		hasBinaryKeys = hasBinaryKeys || rows.HasBinaryKeys()
		hasBinaryVals = hasBinaryVals || rows.HasBinaryVals()

		headers := []string{"Key"}
		if hasBinaryKeys {
			headers = append(headers, "Key (hex)")
		}
		headers = append(headers, "Val")
		if hasBinaryVals {
			headers = append(headers, "Val (hex)")
		}

		table.Clear()
		for i, title := range headers {
			table.SetCell(0, i, &tview.TableCell{
				Text:          title,
				Align:         tview.AlignLeft,
				Color:         tcell.ColorGray,
				Attributes:    tcell.AttrBold,
				NotSelectable: false,
			})
		}

		for i, r := range rows {
			var col int
			table.SetCell(i+1, col, &tview.TableCell{
				Text:  fmt.Sprintf("%v ", displayASCII(r.Key)),
				Align: tview.AlignLeft,
			})
			col++
			if hasBinaryKeys {
				table.SetCell(i+1, col, &tview.TableCell{
					Text:  fmt.Sprintf("% 0x ", r.Key),
					Align: tview.AlignLeft,
				})
				col++
			}
			table.SetCell(i+1, col, &tview.TableCell{
				Text:  fmt.Sprintf("%v ", displayASCII(r.Val)),
				Align: tview.AlignLeft,
			})
			col++
			if hasBinaryVals {
				table.SetCell(i+1, col, &tview.TableCell{
					Text:  fmt.Sprintf("% 0x ", r.Val),
					Align: tview.AlignLeft,
				})
				col++
			}
		}

		if hasNextPage {
			// Sentinel to load next page
			table.SetCell(table.GetRowCount()+1, 0, &tview.TableCell{
				Text:  "",
				Align: tview.AlignLeft,
			})
		}

		table.Select(1, 0)
		if back && !wasFirst {
			table.Select(maxRows-2, 0)
		}
	}

	table.SetSelectionChangedFunc(func(row, column int) {
		_, _, _, h := pages.GetInnerRect()
		maxRows := h - 1
		if row >= (maxRows - 1) {
			nextKey = forwardKey
			nextVal = forwardVal
			updateTable(false)
		} else if row == 0 {
			// FIXME: This only allows going back one page
			updateTable(true)
		}
	})

	table.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEscape:
			pages.SwitchToPage("databases")
		}
	})

	table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyHome:
			nextKey = nil
			nextVal = nil
			updateTable(false)
			return nil
		case tcell.KeyEnd:
			// TODO: Support End
			return nil
		}
		return event
	})

	updateTable(false)

	return
}

func databasesView() error {
	// Setup list of databases view
	databases := tview.NewTable()
	databases.SetBorder(true).SetTitle(" Databases ")
	databases.Clear()
	databases.SetSelectable(true, false)
	databases.SetSelectedStyle(tcell.StyleDefault.
		Background(tcell.ColorWhite).Foreground(tcell.ColorBlack))
	databases.SetBorderPadding(0, 0, 1, 1)
	databases.SetSelectedFunc(func(row, column int) {
		dbiView(databases.GetCell(row, 0).Text)
	})

	for i, title := range []string{
		"Name", "Entries", "Size",
		"BranchPages", "LeafPages", "OverflowPages",
		"Depth", "Flags",
	} {
		databases.SetCell(0, i, &tview.TableCell{
			Text:          title,
			Align:         tview.AlignLeft,
			Color:         tcell.ColorGray,
			Attributes:    tcell.AttrBold,
			NotSelectable: true,
		})
	}

	// Setup pages
	pages.AddPage("databases", databases, true, true)

	// Get list of databases
	err := env.View(func(txn *lmdb.Txn) error {
		rootDBI, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}

		scanner := lmdbscan.New(txn, rootDBI)
		defer scanner.Close()

		row := 1 // 0 is the header
		addDBI := func(name string, dbi lmdb.DBI) {
			st, err := txn.Stat(dbi)
			if err != nil {
				return
			}

			flags, err := txn.Flags(dbi)
			if err != nil {
				return
			}

			databases.SetCell(row, 0, &tview.TableCell{
				Text:  name,
				Align: tview.AlignLeft,
			})
			databases.SetCell(row, 1, &tview.TableCell{
				Text:  humanize.Comma(int64(st.Entries)),
				Align: tview.AlignRight,
			})
			databases.SetCell(row, 2, &tview.TableCell{
				Text:  " " + humanize.Bytes(sizeBytes(st)),
				Align: tview.AlignRight,
			})
			databases.SetCell(row, 3, &tview.TableCell{
				Text:  humanize.Comma(int64(st.BranchPages)),
				Align: tview.AlignRight,
			})
			databases.SetCell(row, 4, &tview.TableCell{
				Text:  humanize.Comma(int64(st.LeafPages)),
				Align: tview.AlignRight,
			})
			databases.SetCell(row, 5, &tview.TableCell{
				Text:  humanize.Comma(int64(st.OverflowPages)),
				Align: tview.AlignRight,
			})
			databases.SetCell(row, 6, &tview.TableCell{
				Text:  humanize.Comma(int64(st.Depth)),
				Align: tview.AlignRight,
			})
			databases.SetCell(row, 7, &tview.TableCell{
				Text:  displayFlags(flags),
				Align: tview.AlignLeft,
			})
			row++
		}

		addDBI(RootDBIName, rootDBI)
		for scanner.Scan() {
			// DBI entries appear to always have 48 bytes, corresponding to
			// the MDB_db struct.
			// TODO: This appears to also contain db flags
			if len(scanner.Val()) != 48 {
				continue
			}
			name := string(scanner.Key())
			dbi, err := txn.OpenDBI(name, 0)
			if err != nil {
				continue
			}
			addDBI(name, dbi)
		}
		databases.Select(1, 0)
		return scanner.Err()

	})
	if err != nil {
		return err
	}

	databases.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEscape:
			app.Stop()
		}
	})

	return err
}

func closeWithLog(c io.Closer) {
	if err := c.Close(); err != nil {
		log.Printf("Error closing %v: %w", c, err)
	}
}

func sizeBytes(st *lmdb.Stat) uint64 {
	return uint64(st.PSize) * (st.BranchPages + st.LeafPages + st.OverflowPages)
}

type KV struct {
	Key, Val []byte
}

type KVList []KV

func (kvl KVList) HasBinaryKeys() bool {
	for _, kv := range kvl {
		if isBinary(kv.Key) {
			return true
		}
	}
	return false
}

func (kvl KVList) HasBinaryVals() bool {
	for _, kv := range kvl {
		if isBinary(kv.Val) {
			return true
		}
	}
	return false
}

func isBinary(b []byte) bool {
	for _, ch := range b {
		if ch < 32 || ch > 127 {
			return true
		}
	}
	return false
}

func displayASCII(b []byte) string {
	ret := make([]byte, len(b))
	for i, ch := range b {
		if ch < 32 || ch > 126 {
			ret[i] = '.'
		} else {
			ret[i] = ch
		}
	}
	return string(ret)
}

func displayFlags(fl uint) string {
	var names []string
	for _, fd := range flagNames {
		if fl&fd.flag > 0 {
			names = append(names, fd.name)
		}
	}
	unknown := fl &^ knownFlags
	if unknown > 0 {
		names = append(names, fmt.Sprintf("%02x", unknown))
	}
	return strings.Join(names, ",")
}

var flagNames = []struct {
	name string
	flag uint
}{
	/*(MDB_REVERSEKEY|MDB_DUPSORT|MDB_INTEGERKEY|MDB_DUPFIXED|\ MDB_INTEGERDUP|MDB_REVERSEDUP|MDB_CREATE) */
	{"REVERSEKEY", lmdb.ReverseKey},
	{"DUPSORT", lmdb.DupSort},
	{"DUPFIXED", lmdb.DupFixed},
	{"REVERSEDUP", lmdb.ReverseDup},
	// Not usable in Go bindings
	{"INTEGERKEY", 0x08},
	{"INTEGERDUP", 0x20},
}

var knownFlags uint = lmdb.ReverseKey | lmdb.DupSort | lmdb.DupFixed | lmdb.ReverseDup | 0x08 | 0x20