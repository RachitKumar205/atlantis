package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// cmdParked — `tide parked`
//
// Lists objects a destructive migration parked instead of dropping, and how
// long each one is still recoverable for.
//
// It answers whether a removed object can still be recovered. The only other
// route to that is a SELECT against atlantis.parked_objects, which no command,
// page or doc names.
func cmdParked(args []string) int {
	fs := flag.NewFlagSet("parked", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	format := fs.String("format", "table", "Output format: table or json")
	all := fs.Bool("all", false, "Include objects that have already been reaped")
	limit := fs.Int("limit", 100, "Maximum rows to return")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.ListParkedObjects(ctx, &adminpb.ListParkedObjectsRequest{
		IncludeReaped: *all,
		Limit:         int32(*limit),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide parked:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide parked:", err)
			return 3
		}
	case "table":
		printParkedTable(resp, *all)
	default:
		fmt.Fprintf(os.Stderr, "tide parked: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

func printParkedTable(resp *adminpb.ListParkedObjectsResponse, all bool) {
	objs := resp.GetObjects()
	if len(objs) == 0 {
		if all {
			fmt.Println(cliout.Grey("(nothing has ever been parked)"))
		} else {
			fmt.Println(cliout.Grey("(nothing is parked)"))
		}
		return
	}

	// cliout.Pad, not %-Ns. fmt pads by bytes and a colour wrapper adds ~23
	// invisible ones, so a coloured cell in a fixed-width column collapses —
	// and because only SOME cells here are coloured (the urgent ones), the
	// columns would jump left and right row to row.
	fmt.Println(cliout.Pad(cliout.Bold("KIND"), 8) +
		cliout.Pad(cliout.Bold("OBJECT"), 46) +
		cliout.Pad(cliout.Bold("RECOVERABLE FOR"), 18) +
		cliout.Bold("STATE"))

	now := time.Now()
	var stuck int
	for _, o := range objs {
		name := o.GetOriginalSchema() + "." + o.GetOriginalName()
		if o.GetKind() == "column" {
			name = o.GetOriginalSchema() + "." + o.GetParentTable() + "." + o.GetOriginalName()
		}

		window, state := "—", cliout.Grey("reaped")
		if o.GetReapedAt() == "" {
			state = cliout.Green("recoverable")
			if reapAfter, err := time.Parse(time.RFC3339, o.GetReapAfter()); err == nil {
				window = humanUntil(reapAfter.Sub(now))
				// Under a week is the window a return from leave still fits
				// inside.
				if reapAfter.Sub(now) < 7*24*time.Hour {
					window = cliout.Yellow(window)
				}
			}
			// One failure is usually a lock the reaper declined to wait for
			// rather than a problem; nothing resets the counter, so flagging at
			// one would mark healthy objects forever.
			if o.GetAttempts() >= stuckAttempts {
				state = cliout.Yellow(fmt.Sprintf("stuck (%d)", o.GetAttempts()))
				stuck++
			}
		}
		fmt.Println(cliout.Pad(o.GetKind(), 8) +
			cliout.Pad(cliout.Cyan(name), 46) +
			cliout.Pad(window, 18) + state)
	}

	if resp.GetHasMore() {
		fmt.Println()
		fmt.Println(cliout.Grey("More rows exist than were returned. Raise --limit to see them."))
	}
	if stuck > 0 {
		fmt.Println()
		fmt.Println(cliout.Yellow(fmt.Sprintf(
			"%d object(s) have failed to drop repeatedly. Usually something was "+
				"created against them after they were parked; the reaper declines "+
				"rather than cascading. Run with --format=json to see last_error.", stuck)))
	}
	if !all {
		fmt.Println()
		fmt.Println(cliout.Grey("Reverting the schema change restores an object. " +
			"See `tide parked --all` for what has already been reaped."))
	}
}

// stuckAttempts is where repeated failure stops being noise. See the comment
// in printParkedTable.
const stuckAttempts = 2

// humanUntil renders a retention window as a short duration, or "due now" once
// it has passed.
func humanUntil(d time.Duration) string {
	if d <= 0 {
		return "due now"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
