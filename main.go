package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/mattn/go-shellwords"
	"github.com/urfave/cli"
)

const version = "0.1"

var (
	username string
	url      string
	color    string
	attach   bool
	prefix   string
	syntax   string
)

func main() {
	app := cli.NewApp()
	app.Name = "mmjournalmon"
	app.Usage = "Monitor logs and send updates to mattermost"
	app.Version = version
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "debug, d",
			Usage: "Enable debugging.",
		},
		cli.StringFlag{
			Name:  "prefix, p",
			Usage: "Prefix for messages.",
			Value: ":warning:",
		},
		cli.StringFlag{
			Name:  "syntax",
			Usage: "Syntax for logs.",
		},
		cli.StringFlag{
			Name:  "username, u",
			Usage: "Username for messaging mattermost",
		},
		cli.StringFlag{
			Name:  "url",
			Usage: "URL for mattermost webhook",
		},
		cli.StringFlag{
			Name:  "color",
			Usage: "Color for mattermost webhook",
			Value: "#FF0000",
		},
		cli.StringFlag{
			Name:  "param, jp",
			Usage: "journalctl parameters",
		},
		cli.StringSliceFlag{
			Name:  "include, i",
			Usage: "Regex pattern of MESSAGES to include.",
		},
		cli.StringSliceFlag{
			Name:  "exclude, x",
			Usage: "Regex pattern of MESSAGES to exclude.",
		},
		cli.BoolFlag{
			Name:  "no-attach",
			Usage: "Post logs as text instead of an attachment.",
		},
	}
	app.Action = Run
	err := app.Run(os.Args)
	if err != nil {
		panic(err)
	}
}

func Run(ctx *cli.Context) error {
	if ctx.Bool("debug") {
		log.SetLevel(log.DebugLevel)
		log.Info("Debug logging enabled")
	}
	username = ctx.String("username")
	url = ctx.String("url")
	color = ctx.String("color")
	prefix = ctx.String("prefix")
	attach = !ctx.Bool("no-attach")

	go mon(ctx)

	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Infof("Signal (%s) received, stopping\n", s)
	return nil
}

var (
	excludes []*regexp.Regexp
	includes []*regexp.Regexp
	matches  []string
	jsplit   *regexp.Regexp
)

func mon(ctx *cli.Context) {
	var err error
	jsplit, err = regexp.Compile("(?m:^[^ ])")
	if err != nil {
		log.WithError(err).Fatal("Error compiling jsplit regex")
	}
	for _, ex := range ctx.StringSlice("exclude") {
		rx, err := regexp.Compile(ex)
		if err != nil {
			log.WithError(err).WithField("exclude", ctx.String("exclude")).Fatal("Error compiling exclude regex")
		}
		excludes = append(excludes, rx)
	}
	for _, in := range ctx.StringSlice("include") {
		ix, err := regexp.Compile(in)
		if err != nil {
			log.WithError(err).WithField("include", ctx.String("include")).Fatal("Error compiling include regex")
		}
		includes = append(includes, ix)
	}

	log.Debug("Starting watch on journal")
	go monJournal(ctx.String("param"))
}

func monJournal(param string) {
	args := []string{"--follow"}
	if len(param) > 0 {
		shellArgs, err := shellwords.Parse(param)
		if err != nil {
			log.WithField("param", param).WithError(err).Fatal("Error parsing parameters")
		}
		args = append(args, shellArgs...)
	}
	cmd := exec.Command("journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err != nil {
		log.WithError(err).Fatal("Failed to setup stdout pipe for journalctl")
	}
	err = cmd.Start()
	if err != nil {
		log.WithError(err).Fatal("Failed to run journalctl")
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Split(splitJournal)
	log.WithField("scanner", scanner).Debug("Scanner setup")

	log.Debug("Starting journal watch")
	for scanner.Scan() {
		msg := scanner.Text()
		//log.WithField("msg", msg).Debug("Recieved message")
		go notify(msg, "journal")
	}
}

func splitJournal(data []byte, atEOF bool) (advance int, token []byte, err error) {
	ai := jsplit.FindAllIndex(data, 2)
	if ai == nil {
		return 0, nil, nil
	}
	log.WithField("len(ai)", len(ai)).Debug("AI matched")
	for _, i := range ai {
		if i[0] == 0 {
			continue
		}
		return i[0], data[0:i[0]], nil
	}
	return len(data), data, nil
}

func notify(lb string, file string) {
	for _, in := range includes {
		if !in.MatchString(lb) {
			return
		}
	}
	for _, ex := range excludes {
		if ex.MatchString(lb) {
			return
		}
	}
	if strings.TrimSpace(lb) == "" {
		return
	}
	log.WithField("lb", lb).Debug("Notify triggered")
	p := make(map[string]interface{})
	p["username"] = username
	if attach {
		a := make(map[string]interface{})
		a["fallback"] = fmt.Sprintf("New log entry in %v. \n%v\n[...]%v\n", file, lb[0], lb[len(lb)-1])
		a["color"] = color
		a["pretext"] = fmt.Sprintf("%v New log entry in %v", prefix, file)
		// TODO: When MM PLT-3340 is fixed, wrap txt in code blocks
		txt := "  " + lb
		a["text"] = txt
		p["attachments"] = []map[string]interface{}{a}
	} else {
		txt := lb
		txt = fmt.Sprintf("%v New log entry in %v\n```%v\n%v\n```", prefix, file, syntax, txt)
		p["text"] = txt
	}
	pj, err := json.Marshal(p)
	if err != nil {
		log.WithError(err).WithField("payload", p).Error("Failed to marshall json")
		return
	}
	log.WithField("payload-json", string(pj)).Debug("Json prepared")
	r, err := http.Post(url, "application/json", bytes.NewBuffer(pj))
	if err != nil {
		if strings.Contains(err.Error(), "REFUSED_STREAM") {
			log.WithError(err).WithField("url", url).WithField("json", string(pj)).Debug("Failed to post json to url. Retrying.")
			time.Sleep(1 * time.Millisecond)
			go notify(lb, file)
			return
		}
		log.WithError(err).WithField("url", url).WithField("json", string(pj)).Error("Failed to post json to url.")
		return
	}
	log.WithField("Response", r).Debug("Response from web hook")
}
