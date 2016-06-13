package putio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultUserAgent = "go-putio"
	defaultMediaType = "application/json"
	defaultBaseURL   = "https://api.put.io"
	defaultUploadURL = "https://upload.put.io"
	defaultSearchURL = "https://put.io"
)

// errors
var (
	ErrNotExist = fmt.Errorf("file does not exist")

	errRedirect   = fmt.Errorf("redirect attempt on a no-redirect client")
	errNegativeID = fmt.Errorf("file id cannot be negative")
)

// Client manages communication with Put.io v2 API.
type Client struct {
	// HTTP client used to communicate with Put.io API
	client *http.Client

	// Base URL for API requests.
	BaseURL *url.URL

	// User agent for client
	UserAgent string
}

// NewClient returns a new Put.io API client, using the htttpClient, which must
// be a new Oauth2 enabled http.Client. If httpClient is not defined, default
// HTTP client is used.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	baseURL, _ := url.Parse(defaultBaseURL)
	c := &Client{
		client:    httpClient,
		BaseURL:   baseURL,
		UserAgent: defaultUserAgent,
	}
	return c
}

// NewRequest creates an API request. A relative URL can be provided via
// relURL, which will be resolved to the BaseURL of the Client.
func (c *Client) NewRequest(method, relURL string, body io.Reader) (*http.Request, error) {
	rel, err := url.Parse(relURL)
	if err != nil {
		return nil, err
	}

	u := c.BaseURL.ResolveReference(rel)
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", defaultMediaType)
	req.Header.Set("User-Agent", c.UserAgent)

	return req, nil
}

// Do sends an API request and returns the API response.
func (c *Client) Do(r *http.Request) (*http.Response, error) {
	return c.client.Do(r)
}

// Get fetches file metadata for given file ID.
func (c *Client) Get(id int) (File, error) {
	if id < 0 {
		return File{}, errNegativeID
	}

	req, err := c.NewRequest("GET", "/v2/files/"+strconv.Itoa(id), nil)
	if err != nil {
		return File{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return File{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return File{}, ErrNotExist
	}

	if resp.StatusCode != http.StatusOK {
		return File{}, fmt.Errorf("get request failed with status: %v", resp.Status)
	}

	var getResponse struct {
		File   File   `json:"file"`
		Status string `json:"status"`
	}
	err = json.NewDecoder(resp.Body).Decode(&getResponse)
	if err != nil {
		return File{}, err
	}
	return getResponse.File, nil
}

// List fetches children for given directory ID.
func (c *Client) List(id int) (FileList, error) {
	if id < 0 {
		return FileList{}, errNegativeID
	}
	req, err := c.NewRequest("GET", "/v2/files/list?parent_id="+strconv.Itoa(id), nil)
	if err != nil {
		return FileList{}, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return FileList{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return FileList{}, ErrNotExist
	}

	if resp.StatusCode != http.StatusOK {
		return FileList{}, fmt.Errorf("list request failed. HTTP Status: %v", resp.Status)
	}

	var listResponse struct {
		Files  []File `json:"files"`
		Parent File   `json:"parent"`
		Status string `json:"status"`
	}

	err = json.NewDecoder(resp.Body).Decode(&listResponse)
	if err != nil {
		return FileList{}, err
	}

	return FileList{
		Files:  listResponse.Files,
		Parent: listResponse.Parent,
	}, nil
}

// Download fetches the contents of the given file. Callers can pass additional
// useTunnel parameter to fetch the file from nearest tunnel server. Storage
// servers accept Range requests, so a range header can be provided by headers
// parameter.
//
// Download request is done by the client which is provided to the NewClient
// constructor. Additional client tunings are taken into consideration while
// downloading a file, such as Timeout etc.
func (c *Client) Download(id int, useTunnel bool, headers http.Header) (io.ReadCloser, error) {
	if id < 0 {
		return nil, errNegativeID
	}

	notunnel := "notunnel=1"
	if useTunnel {
		notunnel = "notunnel=0"
	}

	req, err := c.NewRequest("GET", "/v2/files/"+strconv.Itoa(id)+"/download?"+notunnel, nil)
	if err != nil {
		return nil, err
	}
	// merge headers with request headers
	for header, values := range headers {
		for _, value := range values {
			req.Header.Add(header, value)
		}
	}

	// follow the redirect only once. copy the original request headers to
	// redirect request.
	c.client.CheckRedirect = redirectOnceFunc
	defer func() {
		c.client.CheckRedirect = nil
	}()

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		if resp.Body != nil {
			resp.Body.Close()
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotExist
		}
		return nil, fmt.Errorf("unexpected HTTP status: %v", resp.Status)
	}

	return resp.Body, nil
}

// CreateFolder creates a new folder under parent.
func (c *Client) CreateFolder(name string, parent int) (File, error) {
	if parent < 0 {
		return File{}, errNegativeID
	}

	params := url.Values{}
	params.Set("name", name)
	params.Set("parent_id", strconv.Itoa(parent))

	req, err := c.NewRequest("POST", "/v2/files/create-folder", strings.NewReader(params.Encode()))
	if err != nil {
		return File{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return File{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return File{}, fmt.Errorf("create-folder request failed. HTTP Status: %v", resp.Status)
	}

	var f struct {
		File   File   `json:"file"`
		Status string `json:"status"`
	}
	err = json.NewDecoder(resp.Body).Decode(&f)
	if err != nil {
		return File{}, err
	}

	return f.File, nil
}

// Delete deletes given files.
func (c *Client) Delete(files ...int) error {
	if len(files) == 0 {
		return fmt.Errorf("no file id is given")
	}

	var ids []string
	for _, id := range files {
		if id < 0 {
			return errNegativeID
		}
		ids = append(ids, strconv.Itoa(id))
	}

	params := url.Values{}
	params.Set("file_ids", strings.Join(ids, ","))

	req, err := c.NewRequest("POST", "/v2/files/delete", strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete request failed. HTTP Status: %v", resp.Status)
	}

	var r errorResponse
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return err
	}
	if r.Status != "OK" {
		return fmt.Errorf(r.ErrorMessage)
	}

	return nil
}

// Rename change the name of the file to newname.
func (c *Client) Rename(id int, newname string) error {
	if id < 0 {
		return errNegativeID
	}
	if newname == "" {
		return fmt.Errorf("new filename cannot be empty")
	}

	params := url.Values{}
	params.Set("file_id", strconv.Itoa(id))
	params.Set("name", newname)

	req, err := c.NewRequest("POST", "/v2/files/rename", strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rename request failed. HTTP Status: %v", resp.Status)
	}

	var r errorResponse
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return err
	}
	if r.Status != "OK" {
		return fmt.Errorf(r.ErrorMessage)
	}

	return nil
}

// Move moves files under the destination specified as parent ID.
func (c *Client) Move(parent int, files ...int) error {
	if len(files) == 0 {
		return fmt.Errorf("no file id's are given")
	}

	var ids []string
	for _, id := range files {
		if id < 0 {
			return errNegativeID
		}
		ids = append(ids, strconv.Itoa(id))
	}

	params := url.Values{}
	params.Set("file_ids", strings.Join(ids, ","))
	params.Set("parent", strconv.Itoa(parent))

	req, err := c.NewRequest("POST", "/v2/files/move", strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("move request failed. HTTP Status: %v", resp.Status)
	}

	var r errorResponse
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return err
	}
	if r.Status != "OK" {
		return fmt.Errorf(r.ErrorMessage)
	}

	return nil
}

// Upload reads from fpath and uploads the file contents to Put.io servers
// under the parent directory with the name filename. This method reads the
// file contents into the memory, so it should be used for <150MB files.
//
// If the uploaded file is a torrent file, Put.io v2 API will interprete it as
// a transfer and Transfer field will represent the status of the tranfer.

// Likewise, if the uploaded file is a regular file, Transfer field would be
// nil and the uploaded file will be represented by the File field.
//
// If filename is empty, basename of the fpath will be used.
func (c *Client) Upload(fpath, filename string, parent int) (Upload, error) {
	if parent < 0 {
		return Upload{}, errNegativeID
	}

	if filename == "" {
		filename = filepath.Base(fpath)
	}

	f, err := os.Open(fpath)
	if err != nil {
		return Upload{}, err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	err = mw.WriteField("parent_id", strconv.Itoa(parent))
	if err != nil {
		return Upload{}, err
	}

	formfile, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return Upload{}, err
	}

	_, err = io.Copy(formfile, f)
	if err != nil {
		return Upload{}, err
	}

	err = mw.Close()
	if err != nil {
		return Upload{}, err
	}

	u, _ := url.Parse(defaultUploadURL)
	c.BaseURL = u

	req, err := c.NewRequest("POST", "/v2/files/upload", &buf)
	if err != nil {
		return Upload{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return Upload{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// decode error response
		var errResp errorResponse
		err := json.NewDecoder(resp.Body).Decode(&errResp)
		if err != nil {
			return Upload{}, err
		}
		return Upload{}, fmt.Errorf("Upload failed. %v", errResp)
	}

	var upload struct {
		Upload
		Status string `json"status"`
	}
	err = json.NewDecoder(resp.Body).Decode(&upload)
	if err != nil {
		return Upload{}, err
	}

	if upload.Status == "ERROR" {
		// decode error response
		var errResp errorResponse
		err := json.NewDecoder(resp.Body).Decode(&errResp)
		if err != nil {
			return Upload{}, err
		}
		return Upload{}, fmt.Errorf("Upload failed. %v", errResp)
	}

	return upload.Upload, nil
}

// Search makes a search request with the given query. Servers return 50
// results at a time. The URL for the next 50 results are in Next field.  If
// page is negative, all results are returned.
func (c *Client) Search(query string, page int) (Search, error) {
	if query == "" {
		return Search{}, fmt.Errorf("no query given")
	}

	u, _ := url.Parse(defaultSearchURL)
	c.BaseURL = u

	req, err := c.NewRequest("GET", "/v2/files/search/"+query+"/page/"+strconv.Itoa(page), nil)
	if err != nil {
		return Search{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return Search{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp errorResponse
		err = json.NewDecoder(resp.Body).Decode(&errResp)
		if err != nil {
			return Search{}, err
		}
		return Search{}, fmt.Errorf("search request failed. %v", errResp)
	}

	var s Search
	err = json.NewDecoder(resp.Body).Decode(&s)
	if err != nil {
		return Search{}, err
	}

	return s, nil
}

// File represents a Put.io file.
type File struct {
	ID                int    `json:"id"`
	Filename          string `json:"name"`
	Filesize          int64  `json:"size"`
	ContentType       string `json:"content_type"`
	CreatedAt         string `json:"created_at"`
	FirstAccessedAt   string `json:"first_accessed_at"`
	ParentID          int    `json:"parent_id"`
	Screenshot        string `json:"screenshot"`
	OpensubtitlesHash string `json:"opensubtitles_hash"`
	IsMP4Available    bool   `json:"is_mp4_available"`
	Icon              string `json:"icon"`
	CRC32             string `json:"crc32"`
	IsShared          bool   `json:"is_shared"`
}

// FileList represents a list of files of a Put.io directory.
type FileList struct {
	Files  []File `json:"file"`
	Parent File   `json:"parent"`
}

type Upload struct {
	File     *File     `json:"file"`
	Transfer *Transfer `json:"transfer"`
}

type Search struct {
	Files []File `json:"files"`
	Next  string `json:"next"`
}

type Transfer struct {
	Availability       string `json:"availability"`
	CallbackURL        string `json:"callback_url"`
	CreatedAt          string `json:"created_at"`
	CreatedTorrent     bool   `json:"created_torrent"`
	ClientIP           string `json:"client_ip"`
	CurrentRatio       string `json:"current_ratio"`
	DownloadSpeed      int    `json:"down_speed"`
	Downloaded         int    `json:"downloaded"`
	DownloadID         int    `json:"download_id"`
	ErrorMessage       string `json:"error_message"`
	EstimatedTime      string `json:"estimated_time"`
	Extract            bool   `json:"extract"`
	FileID             int    `json:"file_id"`
	FinishedAt         string `json:"finished_at"`
	ID                 int    `json:"id"`
	IsPrivate          bool   `json:"is_private"`
	MagnetURI          string `json:"magneturi"`
	Name               string `json:"name"`
	PeersConnected     int    `json:"peers_connected"`
	PeersGettingFromUs int    `json:"peers_getting_from_us"`
	PeersSendingToUs   int    `json:"peers_sending_to_us"`
	PercentDone        int    `json:"percent_done"`
	SaveParentID       int    `json:"save_parent_id"`
	SecondsSeeding     int    `json:"seconds_seeding"`
	Size               int    `json:"size"`
	Source             string `json:"source"`
	Status             string `json:"status"`
	StatusMessage      string `json:"status_message"`
	SubscriptionID     int    `json:"subscription_id"`
	TorrentLink        string `json:"torrent_link"`
	TrackerMessage     string `json:"tracker_message"`
	Trackers           string `json:"tracker"`
	Type               string `json:"type"`
	UploadSpeed        int    `json:"up_speed"`
	Uploaded           int    `json:"uploaded"`
}

// errorResponse represents a common error message that Put.io v2 API sends on
// error.
type errorResponse struct {
	ErrorMessage string `json:"error_message"`
	ErrorType    string `json:"error_type"`
	ErrorURI     string `json:"error_uri"`
	Status       string `json:"status"`
	StatusCode   int    `json:"status_code"`
}

func (e errorResponse) Error() string {
	return fmt.Sprintf("StatusCode: %v ErrorType: %v ErrorMsg: %v", e.StatusCode, e.ErrorType, e.ErrorMessage)
}

// redirectOnceFunc follows the redirect only once, and copies the original
// request headers to the new one.
func redirectOnceFunc(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}

	if len(via) > 1 {
		return errRedirect
	}

	// merge headers with request headers
	for header, values := range via[0].Header {
		for _, value := range values {
			req.Header.Add(header, value)
		}
	}
	return nil
}
