package gceidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ComputeAPI is Compute Engine's REST endpoint.
const ComputeAPI = "https://compute.googleapis.com/compute/v1"

// Instances lists a project's instances whose names begin with prefix, in
// every zone, as name to instance ID. client must carry credentials allowed
// compute.instances.list on the project.
//
// The listing must be complete: a zone that could not be read is an error,
// not an absence, because the caller deletes what is absent.
func Instances(ctx context.Context, client *http.Client, base, project, prefix string) (map[string]string, error) {
	if base == "" {
		base = ComputeAPI
	}
	live := map[string]string{}
	page := ""
	for {
		query := url.Values{
			"filter":               {fmt.Sprintf(`name eq "%s.*"`, regexp.QuoteMeta(prefix))},
			"returnPartialSuccess": {"false"},
			"maxResults":           {"500"},
			"fields":               {"items/*/instances(name,id),items/*/warning/code,nextPageToken"},
		}
		if page != "" {
			query.Set("pageToken", page)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			base+"/projects/"+url.PathEscape(project)+"/aggregated/instances?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing instances: %s", response.Status)
		}
		var body struct {
			Items map[string]struct {
				Instances []struct {
					Name string `json:"name"`
					ID   string `json:"id"`
				} `json:"instances"`
				Warning *struct {
					Code string `json:"code"`
				} `json:"warning"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("listing instances: %w", err)
		}
		for scope, item := range body.Items {
			if item.Warning != nil && item.Warning.Code != "NO_RESULTS_ON_PAGE" {
				return nil, fmt.Errorf("listing instances in %s: %s", scope, item.Warning.Code)
			}
			for _, instance := range item.Instances {
				if strings.HasPrefix(instance.Name, prefix) {
					live[instance.Name] = instance.ID
				}
			}
		}
		if body.NextPageToken == "" {
			return live, nil
		}
		page = body.NextPageToken
	}
}
