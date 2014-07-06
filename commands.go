// Copyright (c) 2014 Ludovic Fauvet
// Licensed under the MIT license

package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"io/ioutil"
	"launchpad.net/goyaml"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

var (
	NoSyncMethod = errors.New("Cannot scan a mirror without a proper rsync or FTP url")
)

type cli struct{}

func ParseCommands(args ...string) error {
	c := &cli{}

	if len(args) > 0 && args[0] != "help" {
		method, exists := c.getMethod(args[0])
		if !exists {
			fmt.Println("Error: Command not found:", args[0])
			return c.CmdHelp()
		}
		ret := method.Func.CallSlice([]reflect.Value{
			reflect.ValueOf(c),
			reflect.ValueOf(args[1:]),
		})[0].Interface()
		if ret == nil {
			return nil
		}
		return ret.(error)
	}
	return c.CmdHelp()
}

func (c *cli) getMethod(name string) (reflect.Method, bool) {
	methodName := "Cmd" + strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
	return reflect.TypeOf(c).MethodByName(methodName)
}

func (c *cli) CmdHelp() error {
	help := fmt.Sprintf("Usage: mirrorbits [OPTIONS] COMMAND [arg...]\n\nA smart download redirector.\n\nCommands:\n")
	for _, command := range [][]string{
		{"add", "Add a new mirror"},
		{"disable", "Disable a mirror"},
		{"edit", "Edit a mirror"},
		{"enable", "Enable a mirror"},
		{"export", "Export the mirror database"},
		{"list", "List all mirrors"},
		{"refresh", "Refresh the local repository"},
		{"reload", "Reload configuration"},
		{"remove", "Remove a mirror"},
		{"scan", "(Re-)Scan a mirror"},
		{"upgrade", "Seamless binary upgrade"},
		{"version", "Print version informations"},
	} {
		help += fmt.Sprintf("    %-10.10s%s\n", command[0], command[1])
	}
	fmt.Fprintf(os.Stderr, "%s\n", help)
	return nil
}

func SubCmd(name, signature, description string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintf(os.Stderr, "\nUsage: mirrorbits %s %s\n\n%s\n\n", name, signature, description)
		flags.PrintDefaults()
	}
	return flags
}

func (c *cli) CmdList(args ...string) error {
	cmd := SubCmd("list", "", "Get the list of mirrors")
	http := cmd.Bool("http", false, "Print HTTP addresses")
	rsync := cmd.Bool("rsync", false, "Print rsync addresses")
	ftp := cmd.Bool("ftp", false, "Print FTP addresses")
	state := cmd.Bool("state", true, "Print the state of the mirror")
	disabled := cmd.Bool("disabled", false, "List disabled mirrors only")
	enabled := cmd.Bool("enabled", false, "List enabled mirrors only")
	down := cmd.Bool("down", false, "List only mirrors currently down")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 0 {
		cmd.Usage()
		return nil
	}

	r := NewRedis()
	conn, err := r.connect()
	if err != nil {
		log.Fatal("Redis: ", err)
	}
	defer conn.Close()

	mirrorsIDs, err := redis.Strings(conn.Do("LRANGE", "MIRRORS", "0", "-1"))
	if err != nil {
		log.Fatal("Cannot fetch the list of mirrors:", err)
	}

	conn.Send("MULTI")

	for _, e := range mirrorsIDs {
		conn.Send("HGETALL", fmt.Sprintf("MIRROR_%s", e))
	}

	res, err := redis.Values(conn.Do("EXEC"))
	if err != nil {
		log.Fatal("Redis: ", err)
	}

	w := new(tabwriter.Writer)
	w.Init(os.Stdout, 0, 8, 0, '\t', 0)
	fmt.Fprint(w, "Identifier")
	if *http == true {
		fmt.Fprint(w, "\tHTTP")
	}
	if *rsync == true {
		fmt.Fprint(w, "\tRSYNC")
	}
	if *ftp == true {
		fmt.Fprint(w, "\tFTP")
	}
	if *state == true {
		fmt.Fprint(w, "\tSTATE")
	}
	fmt.Fprint(w, "\n")

	for _, e := range res {
		var mirror Mirror
		res, ok := e.([]interface{})
		if !ok {
			log.Fatal("Typecast failed")
		} else {
			err := redis.ScanStruct([]interface{}(res), &mirror)
			if err != nil {
				log.Fatal("ScanStruct:", err)
			}
			if *disabled == true {
				if mirror.Enabled == true {
					continue
				}
			}
			if *enabled == true {
				if mirror.Enabled == false {
					continue
				}
			}
			if *down == true {
				if mirror.Up == true {
					continue
				}
			}
			fmt.Fprintf(w, "%s", mirror.ID)
			if *http == true {
				fmt.Fprintf(w, "\t%s", mirror.HttpURL)
			}
			if *rsync == true {
				fmt.Fprintf(w, "\t%s", mirror.RsyncURL)
			}
			if *ftp == true {
				fmt.Fprintf(w, "\t%s", mirror.FtpURL)
			}
			if *state == true {
				if mirror.Up == true {
					fmt.Fprintf(w, "\tup   (%s)", time.Unix(mirror.StateSince, 0).Format(time.RFC1123))
				} else {
					fmt.Fprintf(w, "\tdown (%s)", time.Unix(mirror.StateSince, 0).Format(time.RFC1123))
				}
			}
			fmt.Fprint(w, "\n")
		}
	}

	w.Flush()

	return nil
}

func (c *cli) CmdAdd(args ...string) error {
	cmd := SubCmd("add", "[OPTIONS] IDENTIFIER", "Add a new mirror")
	http := cmd.String("http", "", "HTTP base URL")
	rsync := cmd.String("rsync", "", "RSYNC base URL (for scanning only)")
	ftp := cmd.String("ftp", "", "FTP base URL (for scanning only)")
	sponsorName := cmd.String("sponsor-name", "", "Name of the sponsor")
	sponsorURL := cmd.String("sponsor-url", "", "URL of the sponsor")
	sponsorLogo := cmd.String("sponsor-logo", "", "URL of a logo to display for this mirror")
	adminName := cmd.String("admin-name", "", "Admin's name")
	adminEmail := cmd.String("admin-email", "", "Admin's email")
	customData := cmd.String("custom-data", "", "Associated data to return when the mirror is selected (i.e. json document)")
	continentOnly := cmd.Bool("continent-only", false, "The mirror should only handle its continent")
	countryOnly := cmd.Bool("country-only", false, "The mirror should only handle its country")
	asOnly := cmd.Bool("as-only", false, "The mirror should only handle clients in the same AS number")
	score := cmd.Int("score", 0, "Weight to give to the mirror during selection")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}

	if strings.Contains(cmd.Arg(0), " ") {
		fmt.Fprintf(os.Stderr, "The identifier cannot contain a space")
		os.Exit(-1)
	}

	if *http == "" {
		fmt.Fprintf(os.Stderr, "You *must* pass at least an HTTP URL")
		os.Exit(-1)
	}

	u, err := url.Parse(*http)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't parse HTTP url\n")
		os.Exit(-1)
	}

	if !strings.HasPrefix(*http, "http://") {
		*http = "http://" + *http
	}

	ip, err := lookupMirrorIP(u.Host)
	if err == errMultipleAddresses {
		fmt.Fprintf(os.Stderr, "Warning: the hostname returned more than one address! This is highly unreliable.\n")
	}

	geo := NewGeoIP()
	if err := geo.LoadGeoIP(); err != nil {
		log.Fatalf("Can't load GeoIP: %s", err)
	}

	geoRec := geo.GetInfos(ip)

	r := NewRedis()
	conn, err := r.connect()
	if err != nil {
		log.Fatal("Redis: ", err)
	}
	defer conn.Close()

	key := fmt.Sprintf("MIRROR_%s", cmd.Arg(0))
	exists, err := redis.Bool(conn.Do("EXISTS", key))
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(os.Stderr, "Mirror %s already exists!\n", cmd.Arg(0))
		os.Exit(-1)
	}

	// Normalize the URLs
	if http != nil {
		*http = normalizeURL(*http)
	}
	if rsync != nil {
		*rsync = normalizeURL(*rsync)
	}
	if ftp != nil {
		*ftp = normalizeURL(*ftp)
	}

	var latitude, longitude float32
	var continentCode, countryCode string

	if geoRec.GeoIPRecord != nil {
		latitude = geoRec.GeoIPRecord.Latitude
		longitude = geoRec.GeoIPRecord.Longitude
		continentCode = geoRec.GeoIPRecord.ContinentCode
		countryCode = geoRec.GeoIPRecord.CountryCode
	} else {
		fmt.Fprintf(os.Stderr, "Warning: unable to guess the geographic location of %s\n", cmd.Arg(0))
	}

	_, err = conn.Do("HMSET", key,
		"ID", cmd.Arg(0),
		"http", *http,
		"rsync", *rsync,
		"ftp", *ftp,
		"sponsorName", *sponsorName,
		"sponsorURL", *sponsorURL,
		"sponsorLogo", *sponsorLogo,
		"adminName", *adminName,
		"adminEmail", *adminEmail,
		"customData", *customData,
		"continentOnly", *continentOnly,
		"countryOnly", *countryOnly,
		"asOnly", *asOnly,
		"score", *score,
		"latitude", fmt.Sprintf("%f", latitude),
		"longitude", fmt.Sprintf("%f", longitude),
		"continentCode", continentCode,
		"countryCodes", countryCode,
		"asnum", geoRec.ASNum,
		"enabled", false,
		"up", false)
	if err != nil {
		goto oops
	}

	_, err = conn.Do("LPUSH", "MIRRORS", cmd.Arg(0))
	if err != nil {
		goto oops
	}

	// Publish update
	conn.Do("PUBLISH", MIRROR_UPDATE, cmd.Arg(0))

	fmt.Println("Mirror added successfully")
	return nil
oops:
	fmt.Fprintf(os.Stderr, "Oops: %s", err)
	os.Exit(-1)
	return nil
}

func (c *cli) CmdRemove(args ...string) error {
	cmd := SubCmd("remove", "IDENTIFIER", "Remove an existing mirror")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	// Guess which mirror to use
	list, err := c.matchMirror(cmd.Arg(0))
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(os.Stderr, "No match for %s\n", cmd.Arg(0))
		return nil
	} else if len(list) > 1 {
		for _, e := range list {
			fmt.Fprintf(os.Stderr, "%s\n", e)
		}
		return nil
	}

	identifier := list[0]

	r := NewRedis()
	conn, err := r.connect()
	if err != nil {
		log.Fatal("Redis: ", err)
	}
	defer conn.Close()

	// First disable the mirror
	disableMirror(identifier)

	// Get all files supported by the given mirror
	files, err := redis.Strings(conn.Do("SMEMBERS", fmt.Sprintf("MIRROR_%s_FILES", identifier)))
	if err != nil {
		log.Fatal("Error: Cannot fetch file list: ", err)
	}

	conn.Send("MULTI")

	// Remove each FILEINFO / FILEMIRRORS
	for _, file := range files {
		conn.Send("DEL", fmt.Sprintf("FILEINFO_%s_%s", identifier, file))
		conn.Send("SREM", fmt.Sprintf("FILEMIRRORS_%s", file), identifier)
		conn.Send("PUBLISH", MIRROR_FILE_UPDATE, fmt.Sprintf("%s %s", identifier, file))
	}

	_, err = conn.Do("EXEC")
	if err != nil {
		log.Fatal("Error: FILEINFO/FILEMIRRORS keys could not be removed: ", err)
	}

	// Remove all other keys
	_, err = conn.Do("DEL",
		fmt.Sprintf("MIRROR_%s", identifier),
		fmt.Sprintf("MIRROR_%s_FILES", identifier),
		fmt.Sprintf("MIRROR_%s_FILES_TMP", identifier),
		fmt.Sprintf("HANDLEDFILES_%s", identifier))

	if err != nil {
		log.Fatal("Error: MIRROR keys could not be removed: ", err)
	}

	// Remove the last reference
	_, err = conn.Do("LREM", "MIRRORS", 0, identifier)

	if err != nil {
		log.Fatal("Error: Could not remove the reference from key MIRRORS")
	}

	// Publish update
	conn.Do("PUBLISH", MIRROR_UPDATE, identifier)

	fmt.Println("Mirror removed successfully")
	return nil
}

func (c *cli) CmdScan(args ...string) error {
	cmd := SubCmd("scan", "[IDENTIFIER]", "(Re-)Scan a mirror")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	r := NewRedis()
	conn, err := r.connect()
	if err != nil {
		log.Fatal("Redis: ", err)
	}
	defer conn.Close()

	// Check if the local repository has been scanned already
	exists, err := redis.Bool(conn.Do("EXISTS", "FILES"))
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(os.Stderr, "Local repository not index.\nYou should run 'refresh' first!\n")
		os.Exit(-1)
	}

	list, err := c.matchMirror(cmd.Arg(0))
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(os.Stderr, "No match for %s\n", cmd.Arg(0))
		return nil
	} else if len(list) > 1 {
		for _, e := range list {
			fmt.Fprintf(os.Stderr, "%s\n", e)
		}
		return nil
	}

	id := list[0]

	key := fmt.Sprintf("MIRROR_%s", id)
	m, err := redis.Values(conn.Do("HGETALL", key))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot fetch mirror details: %s\n", err)
		return err
	}

	var mirror Mirror
	err = redis.ScanStruct(m, &mirror)
	if err != nil {
		return err
	}

	log.Notice("Scanning %s...", id)

	err = NoSyncMethod

	if mirror.RsyncURL != "" {
		err = Scan().ScanRsync(mirror.RsyncURL, id, nil)
	}
	if err != nil && mirror.FtpURL != "" {
		err = Scan().ScanFTP(mirror.FtpURL, id, nil)
	}
	return err
}

func (c *cli) CmdRefresh(args ...string) error {
	cmd := SubCmd("refresh", "", "Scan the local repository")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 0 {
		cmd.Usage()
		return nil
	}

	err := Scan().ScanSource(nil)
	return err
}

func (c *cli) matchMirror(text string) (list []string, err error) {
	if len(text) == 0 {
		return nil, errors.New("Nothing to match")
	}

	r := NewRedis()
	conn, err := r.connect()
	if err != nil {
		log.Fatal("Redis: ", err)
	}
	defer conn.Close()

	list = make([]string, 0, 0)

	mirrorsIDs, err := redis.Strings(conn.Do("LRANGE", "MIRRORS", "0", "-1"))
	if err != nil {
		return nil, errors.New("Cannot fetch the list of mirrors")
	}

	for _, e := range mirrorsIDs {
		if strings.Contains(e, text) {
			list = append(list, e)
		}
	}
	return
}

func (c *cli) CmdEdit(args ...string) error {
	cmd := SubCmd("edit", "[IDENTIFIER]", "Edit a mirror")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	// Find the editor to use
	editor := os.Getenv("EDITOR")

	if editor == "" {
		log.Fatal("Environment variable $EDITOR not set")
	}

	// Guess which mirror to use
	list, err := c.matchMirror(cmd.Arg(0))
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(os.Stderr, "No match for %s\n", cmd.Arg(0))
		return nil
	} else if len(list) > 1 {
		for _, e := range list {
			fmt.Fprintf(os.Stderr, "%s\n", e)
		}
		return nil
	}

	id := list[0]

	// Connect to the database
	r := NewRedis()
	conn, err := r.connect()
	if err != nil {
		log.Fatal("Redis: ", err)
	}
	defer conn.Close()

	// Get the mirror informations
	key := fmt.Sprintf("MIRROR_%s", id)
	m, err := redis.Values(conn.Do("HGETALL", key))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot fetch mirror details: %s\n", err)
		return err
	}

	var mirror Mirror
	err = redis.ScanStruct(m, &mirror)
	if err != nil {
		return err
	}

	// Generate a yaml configuration string from the struct
	out, err := goyaml.Marshal(mirror)

	// Open a temporary file
	f, err := ioutil.TempFile(os.TempDir(), "edit")
	if err != nil {
		log.Fatal("Cannot create temporary file:", err)
	}
	defer os.Remove(f.Name())
	f.WriteString("# You can now edit this mirror configuration.\n" +
		"# Just save and quit when you're done.\n\n")
	f.WriteString(string(out))
	f.Close()

	// Checksum the original file
	chk, _ := hashFile(f.Name())

	// Launch the editor with the filename as first parameter
	exe := exec.Command(editor, f.Name())
	exe.Stdin = os.Stdin
	exe.Stdout = os.Stdout
	exe.Stderr = os.Stderr

	err = exe.Run()
	if err != nil {
		log.Fatal(err)
	}

	// Read the file back
	out, err = ioutil.ReadFile(f.Name())
	if err != nil {
		log.Fatal("Cannot read file", f.Name())
	}

	// Checksum the file back and compare
	chk2, _ := hashFile(f.Name())
	if chk == chk2 {
		fmt.Println("Aborted")
		return nil
	}

	// Fill the struct from the yaml
	err = goyaml.Unmarshal(out, &mirror)
	if err != nil {
		log.Fatal("Parse error: ", err.Error())
	}

	// Reformat contry codes
	mirror.CountryCodes = strings.Replace(mirror.CountryCodes, ",", " ", -1)
	ccodes := strings.Fields(mirror.CountryCodes)
	mirror.CountryCodes = ""
	for _, c := range ccodes {
		mirror.CountryCodes += strings.ToUpper(c) + " "
	}
	mirror.CountryCodes = strings.TrimRight(mirror.CountryCodes, " ")

	// Reformat continent code
	//FIXME sanitize
	mirror.ContinentCode = strings.ToUpper(mirror.ContinentCode)

	// Normalize URLs
	if mirror.HttpURL != "" {
		mirror.HttpURL = normalizeURL(mirror.HttpURL)
	}
	if mirror.RsyncURL != "" {
		mirror.RsyncURL = normalizeURL(mirror.RsyncURL)
	}
	if mirror.FtpURL != "" {
		mirror.FtpURL = normalizeURL(mirror.FtpURL)
	}

	// Save the values back into redis
	_, err = conn.Do("HMSET", key,
		"ID", id,
		"http", mirror.HttpURL,
		"rsync", mirror.RsyncURL,
		"ftp", mirror.FtpURL,
		"sponsorName", mirror.SponsorName,
		"sponsorURL", mirror.SponsorURL,
		"sponsorLogo", mirror.SponsorLogoURL,
		"adminName", mirror.AdminName,
		"adminEmail", mirror.AdminEmail,
		"customData", mirror.CustomData,
		"continentOnly", mirror.ContinentOnly,
		"countryOnly", mirror.CountryOnly,
		"asOnly", mirror.ASOnly,
		"score", mirror.Score,
		"latitude", mirror.Latitude,
		"longitude", mirror.Longitude,
		"continentCode", mirror.ContinentCode,
		"countryCodes", mirror.CountryCodes,
		"asnum", mirror.Asnum,
		"enabled", mirror.Enabled)

	if err != nil {
		log.Fatal("Couldn't save the configuration into redis:", err)
	}

	// Publish update
	conn.Do("PUBLISH", MIRROR_UPDATE, id)

	fmt.Println("Mirror edited successfully")

	return nil
}

func (c *cli) CmdExport(args ...string) error {
	cmd := SubCmd("export", "[format]", "Export the mirror database.\n\nAvailable formats: mirmon")
	rsync := cmd.Bool("rsync", true, "Export rsync URLs")
	http := cmd.Bool("http", true, "Export http URLs")
	ftp := cmd.Bool("ftp", true, "Export ftp URLs")
	disabled := cmd.Bool("disabled", true, "Export disabled mirrors")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	if cmd.Arg(0) != "mirmon" {
		fmt.Fprintf(os.Stderr, "Unsupported format\n")
		cmd.Usage()
		return nil
	}

	r := NewRedis()
	conn, err := r.connect()
	if err != nil {
		log.Fatal("Redis: ", err)
	}
	defer conn.Close()

	mirrorsIDs, err := redis.Strings(conn.Do("LRANGE", "MIRRORS", "0", "-1"))
	if err != nil {
		log.Fatal("Cannot fetch the list of mirrors:", err)
	}

	conn.Send("MULTI")

	for _, e := range mirrorsIDs {
		conn.Send("HGETALL", fmt.Sprintf("MIRROR_%s", e))
	}

	res, err := redis.Values(conn.Do("EXEC"))
	if err != nil {
		log.Fatal("Redis: ", err)
	}

	w := new(tabwriter.Writer)
	w.Init(os.Stdout, 0, 8, 0, '\t', 0)

	for _, e := range res {
		var mirror Mirror
		res, ok := e.([]interface{})
		if !ok {
			log.Fatal("Typecast failed")
		} else {
			err := redis.ScanStruct([]interface{}(res), &mirror)
			if err != nil {
				log.Fatal("ScanStruct:", err)
			}
			if *disabled == false {
				if mirror.Enabled == false {
					continue
				}
			}
			ccodes := strings.Fields(mirror.CountryCodes)

			urls := make([]string, 0, 3)
			if *rsync == true && mirror.RsyncURL != "" {
				urls = append(urls, mirror.RsyncURL)
			}
			if *http == true && mirror.HttpURL != "" {
				urls = append(urls, mirror.HttpURL)
			}
			if *ftp == true && mirror.FtpURL != "" {
				urls = append(urls, mirror.FtpURL)
			}

			for _, u := range urls {
				fmt.Fprintf(w, "%s\t%s\t%s\n", ccodes[0], u, mirror.AdminEmail)
			}
		}
	}

	w.Flush()

	return nil
}

func (c *cli) CmdEnable(args ...string) error {
	cmd := SubCmd("enable", "[IDENTIFIER]", "Enable a mirror")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	// Guess which mirror to use

	list, err := c.matchMirror(cmd.Arg(0))
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(os.Stderr, "No match for %s\n", cmd.Arg(0))
		return nil
	} else if len(list) > 1 {
		for _, e := range list {
			fmt.Fprintf(os.Stderr, "%s\n", e)
		}
		return nil
	}

	err = enableMirror(list[0])
	if err != nil {
		log.Fatal("Couldn't enable the mirror:", err)
	}

	fmt.Println("Mirror enabled successfully")

	return nil
}

func (c *cli) CmdDisable(args ...string) error {
	cmd := SubCmd("disable", "[IDENTIFIER]", "Disable a mirror")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	// Guess which mirror to use

	list, err := c.matchMirror(cmd.Arg(0))
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(os.Stderr, "No match for %s\n", cmd.Arg(0))
		return nil
	} else if len(list) > 1 {
		for _, e := range list {
			fmt.Fprintf(os.Stderr, "%s\n", e)
		}
		return nil
	}

	err = disableMirror(list[0])
	if err != nil {
		log.Fatal("Couldn't disable the mirror:", err)
	}

	fmt.Println("Mirror disabled successfully")

	return nil
}

func (c *cli) CmdReload(args ...string) error {
	pid := getRemoteProcPid()
	if pid > 0 {
		err := syscall.Kill(pid, syscall.SIGHUP)
		if err != nil {
			log.Error("Unable to reload configuration: %v", err)
		}
	} else {
		log.Error("No pid found. Ensures the server is running.")
	}
	return nil
}

func (c *cli) CmdUpgrade(args ...string) error {
	pid := getRemoteProcPid()
	if pid > 0 {
		err := syscall.Kill(pid, syscall.SIGUSR2)
		if err != nil {
			log.Error("Unable to upgrade binary: %v", err)
		}
	} else {
		log.Error("No pid found. Ensures the server is running.")
	}
	return nil
}

func (c *cli) CmdVersion(args ...string) error {
	printVersion()
	return nil
}
