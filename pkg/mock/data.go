package mock

import (
	"context"
	"math/rand"
	"net/http"
	"strconv"
	"strings"

	"github.com/Borislavv/traefik-http-cache-plugin/pkg/config"
	localesandlanguages "github.com/Borislavv/traefik-http-cache-plugin/pkg/locale"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/model"
)

const (
	minStrLen = 8    // Minimum random string length for GenerateRandomString
	maxStrLen = 1024 // Maximum random string length for GenerateRandomString
)

// GenerateRandomRequests produces a slice of *model.Request for use in tests and benchmarks.
// Each request gets a unique combination of project, domain, language, and tags.
func GenerateRandomRequests(cfg *config.Cache, path []byte, num int) []*model.Request {
	i := 0
	list := make([]*model.Request, 0, num)

	// Iterate over all possible language and project ID combinations until num requests are created
	for {
		for _, lng := range localesandlanguages.LanguageCodeList() {
			for projectID := 1; projectID < 1000; projectID++ {
				if i >= num {
					return list
				}
				req := model.NewTestRequest(
					cfg,
					path,
					map[string][]byte{
						"project[id]":    []byte(strconv.Itoa(projectID)),
						"domain":         []byte("1x001.com"),
						"language":       []byte(lng),
						"choice[name]":   []byte("betting"),
						"choice[choice]": []byte("choice[choice][name]=betting_live&choice[choice][choice][name]=betting_live_null&choice[choice][choice][choice][name]=betting_live_null_" + strconv.Itoa(projectID) + "&choice[choice][choice][choice][choice][name]=betting_live_null_" + strconv.Itoa(projectID) + "_" + strconv.Itoa(projectID) + "&choice[choice][choice][choice][choice][choice][name]=betting_live_null_" + strconv.Itoa(projectID) + "_" + strconv.Itoa(projectID) + "_" + strconv.Itoa(i) + "&choice[choice][choice][choice][choice][choice][choice]=null"),
					},
					map[string][][]byte{
						"Content-Type":     {[]byte("application/json")},
						"CacheBox-Control": {[]byte("max-age=1234567890")},
						"Content-Length":   {[]byte("1234567890")},
						"X-Project-ID":     {[]byte("62")},
						"Accept-Encoding":  {[]byte("gzip"), []byte("br")},
						"X-Real-IP":        {[]byte("192.168.1.10")},
						"User-Agent":       {[]byte("Go-http-client/1.1")},
						"Authorization":    {[]byte("Bearer example.jwt.token")},
						"X-Request-ID":     {[]byte("req-abc123")},
						"Accept-Language":  {[]byte("en-US,en;q=0.9")},
					},
				)
				list = append(list, req)
				i++
			}
		}
	}
}

// GenerateRandomResponses generates a list of *model.Response, each linked to a random request and containing
// random body data. Used for stress/load/benchmark testing of cache systems.
func GenerateRandomResponses(cfg *config.Cache, path []byte, num int) []*model.Response {
	headers := http.Header{}
	headers.Add("Accept", "application/json")
	headers.Add("Content-Type", "application/json")

	list := make([]*model.Response, 0, num)
	for _, req := range GenerateRandomRequests(cfg, path, num) {
		data := model.NewRawData(cfg, path, http.StatusOK, headers, []byte(GenerateRandomString()))
		resp, err := model.NewResponse(
			data, req, cfg,
			func(ctx context.Context) (*model.Data, error) {
				// Dummy revalidator; always returns the same data.
				return data, nil
			},
		)
		if err != nil {
			panic(err)
		}
		list = append(list, resp)
	}
	return list
}

// GenerateRandomString returns a random ASCII string of length between minStrLen and maxStrLen.
func GenerateRandomString() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	length := rand.Intn(maxStrLen-minStrLen+1) + minStrLen

	var sb strings.Builder
	sb.Grow(length)

	for i := 0; i < length; i++ {
		sb.WriteByte(letters[rand.Intn(len(letters))])
	}

	return sb.String()
}
