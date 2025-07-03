package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// sourceMap represents a sourceMap. We only really care about the sources and
// sourcesContent arrays.
type sourceMap struct {
	Version        int      `json:"version"`
	Sources        []string `json:"sources"`
	SourcesContent []string `json:"sourcesContent"`
}

// command line args
type config struct {
	outdir   string     // output directory
	url      string     // sourcemap url
	jsurl    string     // javascript url
	proxy    string     // upstream proxy server
	insecure bool       // skip tls verification
	headers  headerList // additional user-supplied http headers
}

type headerList []string

func (i *headerList) String() string {
	return ""
}

func (i *headerList) Set(value string) error {
	*i = append(*i, value)
	return nil
}

// getSourceMap retrieves a sourcemap from a URL or a local file and returns
// its sourceMap.
func getSourceMap(source string, headers []string, insecureTLS bool, proxyURL url.URL) (m sourceMap, err error) {
	var body []byte
	var client http.Client

	log.Printf("[+] Retrieving Sourcemap from %.1024s...\n", source)

	u, err := url.ParseRequestURI(source)
	if err != nil {
		// If it's a file, read it.
		body, err = os.ReadFile(source)
		if err != nil {
			log.Fatalln(err)
		}
	} else {
		if u.Scheme == "http" || u.Scheme == "https" {
			// If it's a URL, get it.
			req, err := http.NewRequest("GET", u.String(), nil)
			tr := &http.Transport{}

			if err != nil {
				log.Fatalln(err)
			}

			if insecureTLS {
				tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			}

			if proxyURL != (url.URL{}) {
				tr.Proxy = http.ProxyURL(&proxyURL)
			}

			client = http.Client{
				Transport: tr,
			}

			if len(headers) > 0 {
				headerString := strings.Join(headers, "\r\n") + "\r\n\r\n" // squish all the headers together with CRLFs
				log.Printf("[+] Setting the following headers: \n%s", headerString)

				r := bufio.NewReader(strings.NewReader(headerString))
				tpReader := textproto.NewReader(r)
				mimeHeader, err := tpReader.ReadMIMEHeader()

				if err != nil {
					log.Fatalln(err)
				}

				req.Header = http.Header(mimeHeader)
			}

			res, err := client.Do(req)

			if err != nil {
				log.Fatalln(err)
			}

			body, err = io.ReadAll(res.Body)
			defer res.Body.Close()

			if res.StatusCode != 200 && len(body) > 0 {
				log.Printf("[!] WARNING - non-200 status code: %d - Confirm this URL contains valid source map manually!", res.StatusCode)
				log.Printf("[!] WARNING - sourceMap URL request return != 200 - however, body length > 0 so continuing... ")
			}

			if err != nil {
				log.Fatalln(err)
			}
		} else if u.Scheme == "data" {
			urlchunks := strings.Split(u.Opaque, ",")
			if len(urlchunks) < 2 {
				log.Fatalf("[!] Could not parse data URI - expected atleast 2 chunks but got %d\n", len(urlchunks))
			}

			data, err := base64.StdEncoding.DecodeString(urlchunks[1])
			if err != nil {
				log.Fatal("[!] Error base64 decoding", err)
			}

			body = []byte(data)
		} else {
			// If it's a file, read it.
			body, err = os.ReadFile(source)
			if err != nil {
				log.Fatalln(err)
			}
		}
	}
	// Unmarshall the body into the struct.
	log.Printf("[+] Read %d bytes, parsing JSON.\n", len(body))
	err = json.Unmarshal(body, &m)

	if err != nil {
		log.Printf("[!] Error parsing JSON - confirm %s is a valid JS sourcemap", source)
	}

	return
}

// getSourceMapFromJS queries a JavaScript URL, parses its headers and content and looks for sourcemaps
// follows the rules outlined in https://tc39.es/source-map-spec/#linking-generated-code
func getSourceMapFromJS(jsurl string, headers []string, insecureTLS bool, proxyURL url.URL) (m sourceMap, err error) {
	var client http.Client

	log.Printf("[+] Retrieving JavaScript from URL: %s.\n", jsurl)

	// perform the request
	u, err := url.ParseRequestURI(jsurl)
	if err != nil {
		log.Fatal(err)
	}

	req, err := http.NewRequest("GET", u.String(), nil)
	tr := &http.Transport{}

	if err != nil {
		log.Fatalln(err)
	}

	if insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	if proxyURL != (url.URL{}) {
		tr.Proxy = http.ProxyURL(&proxyURL)
	}

	client = http.Client{
		Transport: tr,
	}

	if len(headers) > 0 {
		headerString := strings.Join(headers, "\r\n") + "\r\n\r\n" // squish all the headers together with CRLFs
		log.Printf("[+] Setting the following headers: \n%s", headerString)

		r := bufio.NewReader(strings.NewReader(headerString))
		tpReader := textproto.NewReader(r)
		mimeHeader, err := tpReader.ReadMIMEHeader()

		if err != nil {
			log.Fatalln(err)
		}

		req.Header = http.Header(mimeHeader)
	}

	res, err := client.Do(req)

	if err != nil {
		log.Fatalln(err)
	}

	if res.StatusCode != 200 {
		log.Fatalf("[!] non-200 status code: %d", res.StatusCode)
	}

	var sourceMap string

	// check for SourceMap and X-SourceMap (deprecated) headers
	if sourceMap = res.Header.Get("SourceMap"); sourceMap == "" {
		sourceMap = res.Header.Get("X-SourceMap")
	}

	if sourceMap != "" {
		log.Printf("[.] Found SourceMap URI in response headers: %.1024s...", sourceMap)
	} else {
		// parse the javascript
		body, err := io.ReadAll(res.Body)
		if err != nil {
			log.Fatalln(err)
		}
		defer res.Body.Close()

		// JS file can have multiple source maps in it, but only the last line is valid https://sourcemaps.info/spec.html#h.lmz475t4mvbx
		re := regexp.MustCompile(`\/\/[@#] sourceMappingURL=(.*)`)
		match := re.FindAllSubmatch(body, -1)

		if len(match) != 0 {
			// only the sourcemap at the end of the file should be valid
			sourceMap = string(match[len(match)-1][1])
			log.Printf("[.] Found SourceMap in JavaScript body: %.1024s...", sourceMap)
		}
	}

	// this introduces a forced request bug if the JS file we're parsing is
	// malicious and forces us to make a request out to something dodgy - take care
	if sourceMap != "" {
		var sourceMapURL *url.URL
		// handle absolute/relative rules
		sourceMapURL, err = url.ParseRequestURI(sourceMap)
		if err != nil {
			// relative url...
			sourceMapURL, err = u.Parse(sourceMap)
			if err != nil {
				log.Fatal(err)
			}
		}

		return getSourceMap(sourceMapURL.String(), headers, insecureTLS, proxyURL)
	}

	err = errors.New("[!] No sourcemap URL found")
	return
}

// writeFile writes content to file at path p.
func writeFile(p string, content string) error {
	p = filepath.Clean(p) // Clean the path *after* sanitization

	if _, err := os.Stat(filepath.Dir(p)); os.IsNotExist(err) {
		// Using MkdirAll here is tricky, because even if we fail, we might have
		// created some of the parent directories.
		err = os.MkdirAll(filepath.Dir(p), 0700)
		if err != nil {
			return err
		}
	}

	log.Printf("[+] Writing %d bytes to %s.\n", len(content), p)
	return os.WriteFile(p, []byte(content), 0600)
}

// sanitizeSourcePath removes problematic prefixes and characters from source paths
// to make them valid file system paths across different OS.
func sanitizeSourcePath(p string) string {
	// Remove common problematic prefixes like "webpack://", "webpack:/", "file://", etc.
	// We'll use a single regexp for more flexibility.
	// This regex looks for:
	// ^ - start of string
	// (?: - non-capturing group
	//   (?:webpack|file): - "webpack" or "file" followed by a colon
	//   (?:[/]{2,3})? - two or three slashes (for // or ///)
	// | - OR
	//   ^/ - a single leading slash
	// )
	// We also capture everything after the prefix in Group 1.
	re := regexp.MustCompile(`^(?:(?:webpack|file):(?:[/]{2,3})?|/)(.*)$`)
	matches := re.FindStringSubmatch(p)
	if len(matches) > 1 {
		p = matches[1] // Use the captured part after the prefix
	}

	// Replace any remaining colons with an underscore, e.g., "drive:folder" -> "drive_folder"
	// or parts like "http://example.com/file" might become "http__example.com_file"
	p = strings.ReplaceAll(p, ":", "_")

	// Replace any backslashes with forward slashes for consistency, then clean.
	// filepath.Clean will handle converting to OS-specific path separators.
	p = strings.ReplaceAll(p, "\\", "/")

	// This is crucial: Use filepath.Clean to resolve ".." and "." and normalize slashes.
	p = filepath.Clean(p)

	// After cleaning, if it somehow resulted in a leading ".." that's not intended to go
	// above the output directory, you might want to strip it.
	// For this specific issue (colons in webpack paths), Clean should be sufficient.

	return p
}

func main() {
	var proxyURL url.URL
	var conf config
	var err error

	flag.StringVar(&conf.outdir, "output", "", "Source file output directory - REQUIRED")
	flag.StringVar(&conf.url, "url", "", "URL or path to the Sourcemap file - cannot be used with jsurl")
	flag.StringVar(&conf.jsurl, "jsurl", "", "URL to JavaScript file - cannot be used with url")
	flag.StringVar(&conf.proxy, "proxy", "", "Proxy URL")
	help := flag.Bool("help", false, "Show help")
	flag.BoolVar(&conf.insecure, "insecure", false, "Ignore invalid TLS certificates")
	flag.Var(&conf.headers, "header", "A header to send with the request, similar to curl's -H. Can be set multiple times, EG: \"./sourcemapper --header \"Cookie: session=bar\" --header \"Authorization: blerp\"")
	flag.Parse()

	if *help || (conf.url == "" && conf.jsurl == "") || conf.outdir == "" {
		flag.Usage()
		return
	}

	if conf.jsurl != "" && conf.url != "" {
		log.Println("[!] Both -jsurl and -url supplied")
		flag.Usage()
		return
	}

	if conf.proxy != "" {
		p, err := url.Parse(conf.proxy)
		if err != nil {
			log.Fatal(err)
		}
		proxyURL = *p
	}

	var sm sourceMap

	if conf.url != "" {
		if sm, err = getSourceMap(conf.url, conf.headers, conf.insecure, proxyURL); err != nil {
			log.Fatal(err)
		}
	} else if conf.jsurl != "" {
		if sm, err = getSourceMapFromJS(conf.jsurl, conf.headers, conf.insecure, proxyURL); err != nil {
			log.Fatal(err)
		}
	}

	log.Printf("[+] Retrieved Sourcemap with version %d, containing %d entries.\n", sm.Version, len(sm.Sources))

	if len(sm.Sources) == 0 {
		log.Fatal("No sources found.")
	}

	if len(sm.SourcesContent) == 0 {
		log.Fatal("No source content found.")
	}

	if sm.Version != 3 {
		log.Println("[!] Sourcemap is not version 3. This is untested!")
	}

	// Create the base output directory if it doesn't exist.
	// This ensures the root directory exists before we start creating subdirectories.
	if _, err := os.Stat(conf.outdir); os.IsNotExist(err) {
		err = os.MkdirAll(conf.outdir, 0700) // Use MkdirAll for base directory too
		if err != nil {
			log.Fatal(err)
		}
	}

	for i, sourcePath := range sm.Sources {
		// Sanitize the sourcePath before joining with the output directory.
		cleanedSourcePath := sanitizeSourcePath(sourcePath)

		// This `filepath.Clean` was redundant and sometimes problematic before sanitization.
		// `filepath.Join` and `filepath.Clean` together will handle the OS-specific path separators and normalization.
		scriptPath := filepath.Join(conf.outdir, cleanedSourcePath)
		scriptData := sm.SourcesContent[i]

		err := writeFile(scriptPath, scriptData)
		if err != nil {
			log.Printf("Error writing %s file: %s", scriptPath, err)
		}
	}

	log.Println("[+] Done")
}
