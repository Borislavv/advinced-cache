package mock

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/headers"
	"github.com/Borislavv/advanced-cache/pkg/keys"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/queries"
	"github.com/Borislavv/advanced-cache/pkg/rules"
	"net/http"
	"strconv"
)

// GenerateRandomRequests produces a slice of *model.Request for use in tests and benchmarks.
// Each request gets a unique combination of project, domain, language, and tags.
func GenerateRandomRequests(cfg *config.Cache, path []byte, num int) []*model.Request {
	i := 0
	list := make([]*model.Request, 0, num)

	rule := rules.Match(cfg, path)
	if rule == nil {
		panic("rule not found")
	}

	// Iterate over all possible language and project ID combinations until num requests are created
	for {
		//for _, lng := range localesandlanguages.LanguageCodeList() {
		//for projectID := 1; projectID < 1000; projectID++ {
		if i >= num {
			return list
		}
		q := [][2][]byte{
			{[]byte("project[id]"), []byte("285")},
			{[]byte("domain"), []byte("1x001.com")},
			{[]byte("language"), []byte("en")},
			{[]byte("choice[name]"), []byte("betting")},
			{[]byte("choice[choice][name]"), []byte("betting_live")},
			{[]byte("choice[choice][choice][name]"), []byte("betting_live_null")},
			{[]byte("choice[choice][choice][choice][name]"), []byte("betting_live_null_" + strconv.Itoa(i))},
			{[]byte("choice[choice][choice][choice][choice][name]"), []byte("betting_live_null_" + strconv.Itoa(i) + "_" + strconv.Itoa(i))},
			{[]byte("choice[choice][choice][choice][choice][choice][name]"), []byte("betting_live_null_" + strconv.Itoa(i) + "_" + strconv.Itoa(i) + "_" + strconv.Itoa(i))},
			{[]byte("choice[choice][choice][choice][choice][choice][choice]"), []byte("null")},
		}

		query := queries.FilteredAndSortedKeyQueries(q, rule.CacheKey.QueryBytes)

		h := [][2][]byte{
			{[]byte("Host"), []byte("0.0.0.0:8020")},
			{[]byte("Accept-Encoding"), []byte("gzip, deflate, br")},
			{[]byte("Accept-Language"), []byte("en-US,en;q=0.9")},
			{[]byte("Content-Type"), []byte("application/json")},
		}

		header := headers.FilteredAndSortedKeyHeaders(h, rule.CacheKey.HeadersBytes)

		key, shard := keys.Calculate(path, query, header)
		req := model.NewRequest(rule, key, shard, path, query, header)
		list = append(list, req)
		i++
		//}
		//}
	}
}

func StreamRandomRequests(ctx context.Context, cfg *config.Cache, path []byte, num int) <-chan *model.Request {
	i := 0
	out := make(chan *model.Request)

	rule := rules.Match(cfg, path)
	if rule == nil {
		panic("rule not found")
	}

	go func() {
		defer close(out)

		// Iterate over all possible language and project ID combinations until num requests are created
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if i >= num {
					return
				}
				q := [][2][]byte{
					{[]byte("project[id]"), []byte("285")},
					{[]byte("domain"), []byte("1x001.com")},
					{[]byte("language"), []byte("en")},
					{[]byte("choice[name]"), []byte("betting")},
					{[]byte("choice[choice][name]"), []byte("betting_live")},
					{[]byte("choice[choice][choice][name]"), []byte("betting_live_null")},
					{[]byte("choice[choice][choice][choice][name]"), []byte("betting_live_null_" + strconv.Itoa(i))},
					{[]byte("choice[choice][choice][choice][choice][name]"), []byte("betting_live_null_" + strconv.Itoa(i) + "_" + strconv.Itoa(i))},
					{[]byte("choice[choice][choice][choice][choice][choice][name]"), []byte("betting_live_null_" + strconv.Itoa(i) + "_" + strconv.Itoa(i) + "_" + strconv.Itoa(i))},
					{[]byte("choice[choice][choice][choice][choice][choice][choice]"), []byte("null")},
				}

				query := queries.FilteredAndSortedKeyQueries(q, rule.CacheKey.QueryBytes)

				h := [][2][]byte{
					{[]byte("Host"), []byte("0.0.0.0:8020")},
					{[]byte("Accept-Encoding"), []byte("gzip, deflate, br")},
					{[]byte("Accept-Language"), []byte("en-US,en;q=0.9")},
					{[]byte("Content-Type"), []byte("application/json")},
				}

				header := headers.FilteredAndSortedKeyHeaders(h, rule.CacheKey.HeadersBytes)
				key, shard := keys.Calculate(path, query, header)

				req := model.NewRequest(rule, key, shard, path, query, header)
				out <- req
				i++
			}
		}
	}()

	return out
}

func StreamRandomResponses(ctx context.Context, cfg *config.Cache, path []byte, num int) <-chan *model.Response {
	outCh := make(chan *model.Response)

	go func() {
		defer close(outCh)

		for req := range StreamRandomRequests(ctx, cfg, path, num) {
			header := http.Header{}
			header.Add("Content-Type", "application/json")
			header.Add("Vary", "Accept-Encoding, Accept-Language")

			data := model.NewData(req.Rule(), http.StatusOK, header, ResponseBytes())
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
			outCh <- resp
		}
	}()

	return outCh
}

// GenerateRandomResponses generates a list of *model.Response, each linked to a random request and containing
// random body data. Used for stress/load/benchmark testing of cache systems.
func GenerateRandomResponses(cfg *config.Cache, path []byte, num int) []*model.Response {
	list := make([]*model.Response, 0, num)
	for _, req := range GenerateRandomRequests(cfg, path, num) {
		header := http.Header{}
		header.Add("Content-Type", "application/json")
		header.Add("Vary", "Accept-Encoding, Accept-Language")
		data := model.NewData(req.Rule(), http.StatusOK, header, ResponseBytes())
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

var responseBytes = []byte(`{
  "data": {
    "type": "seo/pagedata",
    "attributes": {
      "title": "1xBet: It repeats some phrases multiple times. This is a long description for SEO page data.",
      "description": "This is a long description for SEO page data. This description is intentionally made verbose to increase the JSON payload size.",
      "metaRobots": [],
      "hierarchyMetaRobots": [
        {
          "name": "robots",
          "content": "noindex, nofollow"
        }
      ],
      "ampPageUrl": null,
      "alternativeLinks": [],
      "alternateMedia": [],
      "customCanonical": null,
      "metas": [],
      "siteName": null
    }
  }
}
`)

// ResponseBytes returns a random ASCII string of length between minStrLen and maxStrLen.
func ResponseBytes() []byte {
	length := len(responseBytes)
	response := make([]byte, length)
	copy(response, responseBytes)
	return response
}
