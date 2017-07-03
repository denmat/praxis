package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/convox/praxis/sdk/rack"
	"github.com/convox/praxis/stdcli"
	cli "gopkg.in/urfave/cli.v1"
)

func init() {
	stdcli.RegisterCommand(cli.Command{
		Name:        "login",
		Description: "login into a rack",
		Action:      runLogin,
	})
	stdcli.RegisterCommand(cli.Command{
		Name:        "racks",
		Description: "list of racks available",
		Action:      runRacks,
	})
}

type Login struct {
	ApiKey string `json:"api_key"`
	Error  string `json:"error"`
	Host   string `json:"host"`
}

func runLogin(c *cli.Context) error {
	var console string

	// TODO: Use proxy to login instead of the webui?

	if len(c.Args()) < 1 {
		var err error
		console, err = consoleHost()
		if err != nil {
			return stdcli.Error(err)
		}
	} else {
		console = c.Args()[0]
	}

	fmt.Printf("Email: ")

	reader := bufio.NewReader(os.Stdin)
	email, err := reader.ReadString('\n')
	if err != nil {
		return stdcli.Error(err)
	}

	email = strings.TrimSpace(email)
	if email == "" {
		return stdcli.Errorf("Please provide a valid email")
	}

	fmt.Printf("Password: ")

	pass, err := terminal.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return stdcli.Error(err)
	}

	fmt.Println()
	stdcli.Startf("Authenticating with <name>%s</name>", console)

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	var client = &http.Client{
		Timeout:   time.Second * 10,
		Transport: transport,
	}

	u := &url.URL{
		Host:   console,
		Path:   "/auth/api_key",
		Scheme: "https",
	}

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return stdcli.Error(err)
	}

	req.SetBasicAuth(email, string(pass))

	response, err := client.Do(req)
	if err != nil {
		return stdcli.Error(err)
	}
	defer response.Body.Close()

	data, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return stdcli.Error(err)
	}

	p := Login{}

	if err := json.Unmarshal(data, &p); err != nil {
		return stdcli.Error(fmt.Errorf("login: %s", err.Error()))
	}

	if p.Error != "" {
		return stdcli.Errorf(p.Error)
	}

	if err := setConsoleHost(console); err != nil {
		return stdcli.Error(err)
	}

	u, err = url.Parse(p.Host)
	if err != nil {
		return stdcli.Error(err)
	}

	u.Scheme = "https"
	u.User = url.UserPassword(p.ApiKey, "")

	if err := setConsoleProxy(u.String()); err != nil {
		return stdcli.Error(err)
	}

	stdcli.OK()
	return nil
}

func runRacks(c *cli.Context) error {
	racks, err := ConsoleProxy().Racks()
	if err != nil {
		return stdcli.Error(err)
	}

	racks = append(racks, "local")

	t := stdcli.NewTable("RACKS")

	for _, r := range racks {
		t.AddRow(r)
	}

	t.Print()

	return nil
}

type ProxyClient struct {
	c *rack.Client
}

func ConsoleProxy() *ProxyClient {
	proxy, err := consoleProxy()
	if err != nil {
		fmt.Fprint(os.Stderr, stdcli.Error(err))
		os.Exit(1)
	}

	if proxy == nil {
		fmt.Fprint(os.Stderr, stdcli.Error(errMissingProxyEndpoint))
		os.Exit(1)
	}

	return &ProxyClient{
		c: &rack.Client{Debug: os.Getenv("CONVOX_DEBUG") == "true", Endpoint: proxy, Version: "dev"},
	}
}

func (p *ProxyClient) Racks() (racks []string, err error) {
	err = p.c.Get("/racks", rack.RequestOptions{}, &racks)
	return
}