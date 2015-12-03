package seaweed

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/levenlabs/dank/config"
	"github.com/levenlabs/go-llog"
	"github.com/levenlabs/go-srvclient"
	"io"
	"io/ioutil"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AssignResult holds the result of the assign call to seaweed. It exposes
// two methods to get the Filename and the URL
type AssignResult struct {
	fid string
	url string
}

// rawAssignResult is only used to Unmarshal into and then an AssignResult is
// made to publicly return
type rawAssignResult struct {
	FID string `json:"fid"`
	URL string `json:"url"`
}

type lookupResult struct {
	Locations []location `json:"locations"`
}

type location struct {
	URL string `json:"url"`
}

//todo: RawURLEncoding
var encoder = base64.URLEncoding
var ErrorNotFound = errors.New("not found")

func init() {
	if config.SeaweedAddr == "" {
		llog.Fatal("--seaweed-addr is required")
	}
	rand.Seed(time.Now().UnixNano())
}

// Returns the filename useful for uploading. It's base64-encoded to ensure url
// acceptance and to hide any seaweed formatting
func (r *AssignResult) Filename() string {
	return encoder.EncodeToString([]byte(r.fid))
}

// Returns the host:port of the seaweed volume that contains this file. This is
// only exposed for hiding this value in the signature
func (r *AssignResult) URL() string {
	return r.url
}

// assignResult returns a public AssignResult from a rawAssignResult
func (r *rawAssignResult) assignResult() *AssignResult {
	return &AssignResult{
		fid: r.FID,
		url: r.URL,
	}
}

// decodes the filename and strips off any file extension and un-base64's the
// filename to get the fid
func decodeFilename(f string) (string, error) {
	parts := strings.Split(f, ".")
	fid, err := encoder.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	return string(fid), nil
}

// NewResult returns a AssignResult from a url and filename. This is used when
// a signature is decoded
func NewResult(u, filename string) (*AssignResult, error) {
	fid, err := decodeFilename(filename)
	if err != nil {
		return nil, err
	}
	return &AssignResult{
		fid: fid,
		url: u,
	}, nil
}

func doReq(req *http.Request, expectedCode int, kv llog.KV) (*http.Response, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		kv["error"] = err
		llog.Warn("error making seaweed http request", kv)
		return nil, err
	}
	if err = handleResp(resp, expectedCode, kv); err != nil {
		//return nil here since the handleResp closed the body already
		return nil, err
	}
	return resp, nil
}

func handleResp(resp *http.Response, expectedCode int, kv llog.KV) error {
	if resp.StatusCode != expectedCode {
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			kv["body"] = body
		}
		kv["status"] = resp.Status
		// a not found status should just be debug since its somewhat expected
		if resp.StatusCode == http.StatusNotFound {
			llog.Debug("invalid seaweed status", kv)
			return ErrorNotFound
		}
		llog.Warn("invalid seaweed status", kv)
		return errors.New("unexpected seaweed status")
	}
	return nil
}

// Assign makes an assign call to seaweed to get a filename that can be uploaded
// to and returns an AssignResult. Optionally replication can be sent to
// guarantee the replication of the file and ttl can be sent to expire the file
// after a specific amount of time. See the seaweedfs docs.
func Assign(replication, ttl string) (*AssignResult, error) {
	addr := srvclient.MaybeSRV(config.SeaweedAddr)
	uStr := "http://" + addr + "/dir/assign"
	u, err := url.Parse(uStr)
	if err != nil {
		llog.Error("error building seaweed url", llog.KV{
			"addr": addr,
		})
		return nil, err
	}
	q := u.Query()
	if replication != "" {
		q.Set("replication", replication)
	}
	if ttl != "" {
		q.Set("ttl", ttl)
	}
	u.RawQuery = q.Encode()
	uStr = u.String()

	kv := llog.KV{
		"url": uStr,
	}
	llog.Debug("making seaweed GET request", kv)

	resp, err := http.Get(uStr)
	if err != nil {
		kv["error"] = err
		llog.Warn("error making seaweed http request", kv)
		return nil, err
	}
	if err = handleResp(resp, http.StatusOK, kv); err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	r := &rawAssignResult{}
	err = json.NewDecoder(resp.Body).Decode(r)
	if err != nil {
		kv["error"] = err
		llog.Error("error decoding assign response from seaweed", kv)
		return nil, err
	}
	return r.assignResult(), nil
}

// Upload takes an existing AssignResult call that has already been validated
// and a io.Reader body. It uploads the body to the sent seaweed volume and
// fid. Optionally it passes along a ttl to seaweed.
func Upload(r *AssignResult, body io.Reader, ttl string) error {
	u, err := url.Parse("http://" + r.url + "/" + r.fid)
	if err != nil {
		llog.Error("error building seaweed url", llog.KV{
			"url": r.url,
			"fid": r.fid,
		})
		return err
	}
	q := u.Query()
	if ttl != "" {
		q.Set("ttl", ttl)
	}
	u.RawQuery = q.Encode()
	uStr := u.String()
	kv := llog.KV{
		"url": uStr,
	}
	llog.Debug("making seaweed PUT request", kv)

	// we HAVE to upload a form the file in file
	newBody := &bytes.Buffer{}
	mpw := multipart.NewWriter(newBody)
	part, err := mpw.CreateFormFile("file", r.Filename())
	if err != nil {
		kv["error"] = err
		kv["filename"] = r.Filename()
		llog.Error("error creating multipart file", kv)
		return err
	}
	_, err = io.Copy(part, body)
	if err != nil {
		kv["error"] = err
		llog.Error("error copying body to multipart", kv)
		return err
	}
	err = mpw.Close()
	if err != nil {
		kv["error"] = err
		llog.Error("error closing multipart writer", kv)
		return err
	}

	req, err := http.NewRequest("PUT", uStr, newBody)
	if err != nil {
		kv["error"] = err
		llog.Warn("error making seaweed http request", kv)
		return err
	}
	req.Header.Add("Content-Type", mpw.FormDataContentType())
	var resp *http.Response
	if resp, err = doReq(req, http.StatusCreated, kv); err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func lookup(filename string) (string, error) {
	fid, err := decodeFilename(filename)
	if err != nil {
		llog.Error("error decoding filename in lookup", llog.KV{
			"filename": filename,
		})
		return "", err
	}
	//fid's format is volumeId,somestuff
	parts := strings.Split(fid, ",")
	addr := srvclient.MaybeSRV(config.SeaweedAddr)
	uStr := "http://" + addr + "/dir/lookup?volumeId=" + parts[0]

	kv := llog.KV{
		"url":  uStr,
		"addr": addr,
	}
	llog.Debug("making seaweed GET request", kv)

	resp, err := http.Get(uStr)
	if err != nil {
		kv["error"] = err
		llog.Warn("error making seaweed http request", kv)
		return "", err
	}
	if err = handleResp(resp, http.StatusOK, kv); err != nil {
		return "", err
	}
	defer resp.Body.Close()

	r := &lookupResult{}
	err = json.NewDecoder(resp.Body).Decode(r)
	if err != nil {
		kv["error"] = err
		llog.Error("error decoding get response from seaweed", kv)
		return "", err
	}
	if len(r.Locations) == 0 {
		return "", ErrorNotFound
	}
	i := rand.Intn(len(r.Locations))
	u := r.Locations[i].URL
	uStr = "http://" + u + "/" + fid
	return uStr, nil
}

// Get takes the given filename, gets the file from seaweed, and writes it to
// the passed io.Writer
func Get(filename string, w io.Writer) (*http.Header, error) {
	uStr, err := lookup(filename)
	if err != nil {
		return nil, err
	}
	kv := llog.KV{
		"url":      uStr,
		"filename": filename,
	}
	llog.Debug("making seaweed GET request", kv)

	resp, err := http.Get(uStr)
	if err != nil {
		kv["error"] = err
		llog.Warn("error making seaweed http request", kv)
		return nil, err
	}
	if err = handleResp(resp, http.StatusOK, kv); err != nil {
		return &resp.Header, err
	}
	defer resp.Body.Close()

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		kv["error"] = err
		llog.Error("error copying body to writer", kv)
	}
	return &resp.Header, err
}

// Delete takes the given filename and deletes it from seaweed
func Delete(filename string) error {
	uStr, err := lookup(filename)
	if err != nil {
		return err
	}
	kv := llog.KV{
		"url":      uStr,
		"filename": filename,
	}
	llog.Debug("making seaweed DELETE request", kv)

	req, err := http.NewRequest("DELETE", uStr, nil)
	if err != nil {
		kv["error"] = err
		llog.Warn("error making seaweed http request", kv)
		return err
	}
	var resp *http.Response
	if resp, err = doReq(req, http.StatusAccepted, kv); err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
