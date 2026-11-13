// Command cx-log prints Amazon Connect Agentic CX Designer workspace logs,
// like tail(1): the last N events, optionally followed (-f) as new events
// arrive. It uses QueryLogs from the acxd Go SDK.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/megaproaktiv/agentic-cx-designer-sdk-go/acxd"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			return // Ctrl-C while following is a normal exit
		}
		fmt.Fprintf(os.Stderr, "cx-log: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	envPath, explicit := envFileFromArgs(args)
	if err := loadDotEnv(envPath, explicit); err != nil {
		return err
	}

	fs := flag.NewFlagSet("cx-log", flag.ContinueOnError)
	fs.Usage = func() { usage(fs) }

	fs.String("env-file", ".env", "`file` with ACXD_* settings (environment variables take precedence)")
	region := fs.String("region", os.Getenv("ACXD_REGION"), "AWS region, e.g. eu-central-1 (or ACXD_REGION)")
	// The API key is deliberately not a flag: flags leak into shell history and ps.
	apiKey := os.Getenv("ACXD_API_KEY")
	workspaceID := fs.String("workspace-id", os.Getenv("ACXD_WORKSPACE_ID"), "workspace ID (or ACXD_WORKSPACE_ID)")
	endpoint := fs.String("endpoint-url", os.Getenv("ACXD_ENDPOINT_URL"), "override the region-derived API endpoint")

	var o tailOptions
	fs.IntVar(&o.Lines, "n", 10, "print the last `N` events (0 = none)")
	fs.BoolVar(&o.Follow, "f", false, "follow: keep polling and print new events as they arrive")
	fs.DurationVar(&o.Since, "since", time.Hour, "how far back to look for the initial -n events")
	fs.DurationVar(&o.Interval, "interval", 5*time.Second, "poll interval with -f")
	fs.DurationVar(&o.Lag, "lag", 2*time.Minute, "with -f, re-scan this far back each poll to catch late-ingested events")
	fs.IntVar(&o.PageSize, "page-size", 100, "QueryLogs page size (5-100)")

	appID := fs.String("application-id", "", "only events for this application ID")
	convID := fs.String("conversation-id", "", "only events for this conversation ID")
	corrID := fs.String("correlation-id", "", "only events for this correlation ID")
	deplID := fs.String("deployment-id", "", "only events for this deployment ID")
	buildID := fs.String("build-id", "", "only events for this build ID")
	userID := fs.String("user-id", "", "only events for this end-user ID")
	lang := fs.String("language", "", "only events for this language code, e.g. en-US")
	eventTypes := fs.String("event-type", "", "comma-separated event types to show, e.g. Error,ModelError (filtered locally)")

	output := fs.String("o", "text", "output format: text | json (one JSON object per line)")
	verbose := fs.Bool("v", false, "text output: also print common properties")
	fs.BoolVar(&o.Debug, "debug", false, "print query status per page to stderr")
	noColor := fs.Bool("no-color", false, "disable colored text output (also honors NO_COLOR)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (see -h)", fs.Arg(0))
	}

	switch {
	case *region == "" && *endpoint == "":
		return errors.New("region is required (--region or ACXD_REGION)")
	case apiKey == "":
		return fmt.Errorf("api key is required: set ACXD_API_KEY in %s or the environment", envPath)
	case *workspaceID == "":
		return errors.New("workspace id is required (--workspace-id or ACXD_WORKSPACE_ID)")
	case o.Lines < 0:
		return errors.New("-n must be >= 0")
	case o.Since <= 0 || o.Interval <= 0 || o.Lag < 0:
		return errors.New("--since and --interval must be > 0, --lag must be >= 0")
	case o.PageSize < 5 || o.PageSize > 100:
		return errors.New("--page-size must be between 5 and 100")
	}

	o.Filter = buildSearchFilter(*appID, *convID, *corrID, *deplID, *buildID, *userID, *lang)
	o.EventTypes = parseEventTypes(*eventTypes)

	switch *output {
	case "text":
		color := !*noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
		o.Printer = &textPrinter{w: os.Stdout, verbose: *verbose, color: color}
	case "json":
		o.Printer = &jsonPrinter{w: os.Stdout}
	default:
		return fmt.Errorf("unknown output format %q (text|json)", *output)
	}

	optFns := []func(*acxd.Options){
		acxd.WithRegion(*region),
		acxd.WithAPIKey(apiKey),
		acxd.WithWorkspaceID(*workspaceID),
	}
	if *endpoint != "" {
		optFns = append(optFns, acxd.WithEndpointURL(*endpoint))
	}
	client := acxd.New(optFns...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return newTailer(client, o).Run(ctx)
}

func buildSearchFilter(appID, convID, corrID, deplID, buildID, userID, lang string) *acxd.LogSearchFilter {
	f := &acxd.LogSearchFilter{LanguageCode: acxd.LanguageCode(lang)}
	set := func(dst **string, v string) {
		if v != "" {
			*dst = &v
		}
	}
	set(&f.ApplicationId, appID)
	set(&f.ConversationId, convID)
	set(&f.CorrelationId, corrID)
	set(&f.DeploymentId, deplID)
	set(&f.BuildId, buildID)
	set(&f.UserId, userID)
	if *f == (acxd.LogSearchFilter{}) {
		return nil
	}
	return f
}

func parseEventTypes(s string) map[acxd.LogEventType]bool {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	m := map[acxd.LogEventType]bool{}
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			m[acxd.LogEventType(t)] = true
		}
	}
	return m
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func usage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `cx-log - tail Amazon Connect Agentic CX Designer logs

Usage:
  cx-log [flags]

Examples:
  cx-log                                  # last 10 events from the past hour
  cx-log -n 50 -f                         # last 50 events, then follow
  cx-log -f -n 0 --conversation-id abc    # only new events of one conversation
  cx-log -f --event-type Error,ModelError,DataRequestFailure
  cx-log -n 200 --since 24h -o json | jq .

Settings are read from ./.env (or --env-file), then the environment:
  ACXD_REGION=eu-central-1
  ACXD_WORKSPACE_ID=...
  ACXD_API_KEY=acxd_live_<prefix>.<secret>
Environment variables override the file; flags override both.

Flags:
`)
	fs.PrintDefaults()
}
