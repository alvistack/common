package libimage

import (
	"context"
	"fmt"
	"strings"
	"sync"

	dockerTransport "github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/pkg/sysregistriesv2"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
)

const (
	searchTruncLength = 44
	searchMaxQueries  = 25
	// Let's follow Firefox by limiting parallel downloads to 6.  We do the
	// same when pulling images in c/image.
	searchMaxParallel = int64(6)
)

// SearchResult is holding image-search related data.
type SearchResult struct {
	// Index is the image index (e.g., "docker.io" or "quay.io")
	Index string
	// Name is the canonical name of the image (e.g., "docker.io/library/alpine").
	Name string
	// Description of the image.
	Description string
	// Stars is the number of stars of the image.
	Stars int
	// Official indicates if it's an official image.
	Official string
	// Automated indicates if the image was created by an automated build.
	Automated string
	// Tag is the image tag
	Tag string
}

// SearchOptions customize searching images.
type SearchOptions struct {
	// Filter allows to filter the results.
	Filter SearchFilter
	// Limit limits the number of queries per index (default: 25). Must be
	// greater than 0 to overwrite the default value.
	Limit int
	// NoTrunc avoids the output to be truncated.
	NoTrunc bool
	// Authfile is the path to the authentication file.
	Authfile string
	// InsecureSkipTLSVerify allows to skip TLS verification.
	InsecureSkipTLSVerify types.OptionalBool
	// ListTags returns the search result with available tags
	ListTags bool
}

// SearchFilter allows filtering images while searching.
type SearchFilter struct {
	// Stars describes the minimal amount of starts of an image.
	Stars int
	// IsAutomated decides if only images from automated builds are displayed.
	IsAutomated types.OptionalBool
	// IsOfficial decides if only official images are displayed.
	IsOfficial types.OptionalBool
}

func (r *Runtime) Search(ctx context.Context, term string, options SearchOptions) ([]SearchResult, error) {
	searchRegistries, err := sysregistriesv2.UnqualifiedSearchRegistries(&r.systemContext)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("Searching images matching term %s at the following registries %s", term, searchRegistries)

	// Try to extract a registry from the specified search term.  We
	// consider everything before the first slash to be the registry.  Note
	// that we cannot use the reference parser from the containers/image
	// library as the search term may container arbitrary input such as
	// wildcards.  See bugzilla.redhat.com/show_bug.cgi?id=1846629.
	if spl := strings.SplitN(term, "/", 2); len(spl) > 1 {
		searchRegistries = append(searchRegistries, spl[0])
		term = spl[1]
	}

	// searchOutputData is used as a return value for searching in parallel.
	type searchOutputData struct {
		data []SearchResult
		err  error
	}

	sem := semaphore.NewWeighted(searchMaxParallel)
	wg := sync.WaitGroup{}
	wg.Add(len(searchRegistries))
	data := make([]searchOutputData, len(searchRegistries))

	for i := range searchRegistries {
		if err := sem.Acquire(ctx, 1); err != nil {
			return nil, err
		}
		index := i
		go func() {
			defer sem.Release(1)
			defer wg.Done()
			searchOutput, err := r.searchImageInRegistry(ctx, term, searchRegistries[index], options)
			data[index] = searchOutputData{data: searchOutput, err: err}
		}()
	}

	wg.Wait()
	results := []SearchResult{}
	var multiErr error
	for _, d := range data {
		if d.err != nil {
			multiErr = multierror.Append(multiErr, d.err)
			continue
		}
		results = append(results, d.data...)
	}

	// Optimistically assume that one successfully searched registry
	// includes what the user is looking for.
	if len(results) > 0 {
		return results, nil
	}
	return results, multiErr
}

func (r *Runtime) searchImageInRegistry(ctx context.Context, term, registry string, options SearchOptions) ([]SearchResult, error) {
	// Max number of queries by default is 25
	limit := searchMaxQueries
	if options.Limit > 0 {
		limit = options.Limit
	}

	sys := r.systemContext
	if options.InsecureSkipTLSVerify != types.OptionalBoolUndefined {
		sys.DockerInsecureSkipTLSVerify = options.InsecureSkipTLSVerify
	}

	if options.ListTags {
		results, err := searchRepositoryTags(ctx, &sys, registry, term, options)
		if err != nil {
			return []SearchResult{}, err
		}
		return results, nil
	}

	results, err := dockerTransport.SearchRegistry(ctx, &sys, registry, term, limit)
	if err != nil {
		return []SearchResult{}, err
	}
	index := registry
	arr := strings.Split(registry, ".")
	if len(arr) > 2 {
		index = strings.Join(arr[len(arr)-2:], ".")
	}

	// limit is the number of results to output
	// if the total number of results is less than the limit, output all
	// if the limit has been set by the user, output those number of queries
	limit = searchMaxQueries
	if len(results) < limit {
		limit = len(results)
	}
	if options.Limit != 0 {
		limit = len(results)
		if options.Limit < len(results) {
			limit = options.Limit
		}
	}

	paramsArr := []SearchResult{}
	for i := 0; i < limit; i++ {
		// Check whether query matches filters
		if !(options.Filter.matchesAutomatedFilter(results[i]) && options.Filter.matchesOfficialFilter(results[i]) && options.Filter.matchesStarFilter(results[i])) {
			continue
		}
		official := ""
		if results[i].IsOfficial {
			official = "[OK]"
		}
		automated := ""
		if results[i].IsAutomated {
			automated = "[OK]"
		}
		description := strings.ReplaceAll(results[i].Description, "\n", " ")
		if len(description) > 44 && !options.NoTrunc {
			description = description[:searchTruncLength] + "..."
		}
		name := registry + "/" + results[i].Name
		if index == "docker.io" && !strings.Contains(results[i].Name, "/") {
			name = index + "/library/" + results[i].Name
		}
		params := SearchResult{
			Index:       index,
			Name:        name,
			Description: description,
			Official:    official,
			Automated:   automated,
			Stars:       results[i].StarCount,
		}
		paramsArr = append(paramsArr, params)
	}
	return paramsArr, nil
}

func searchRepositoryTags(ctx context.Context, sys *types.SystemContext, registry, term string, options SearchOptions) ([]SearchResult, error) {
	dockerPrefix := "docker://"
	imageRef, err := alltransports.ParseImageName(fmt.Sprintf("%s/%s", registry, term))
	if err == nil && imageRef.Transport().Name() != dockerTransport.Transport.Name() {
		return nil, errors.Errorf("reference %q must be a docker reference", term)
	} else if err != nil {
		imageRef, err = alltransports.ParseImageName(fmt.Sprintf("%s%s", dockerPrefix, fmt.Sprintf("%s/%s", registry, term)))
		if err != nil {
			return nil, errors.Errorf("reference %q must be a docker reference", term)
		}
	}
	tags, err := dockerTransport.GetRepositoryTags(ctx, sys, imageRef)
	if err != nil {
		return nil, errors.Errorf("error getting repository tags: %v", err)
	}
	limit := searchMaxQueries
	if len(tags) < limit {
		limit = len(tags)
	}
	if options.Limit != 0 {
		limit = len(tags)
		if options.Limit < limit {
			limit = options.Limit
		}
	}
	paramsArr := []SearchResult{}
	for i := 0; i < limit; i++ {
		params := SearchResult{
			Name: imageRef.DockerReference().Name(),
			Tag:  tags[i],
		}
		paramsArr = append(paramsArr, params)
	}
	return paramsArr, nil
}

func (f *SearchFilter) matchesStarFilter(result dockerTransport.SearchResult) bool {
	return result.StarCount >= f.Stars
}

func (f *SearchFilter) matchesAutomatedFilter(result dockerTransport.SearchResult) bool {
	if f.IsAutomated != types.OptionalBoolUndefined {
		return result.IsAutomated == (f.IsAutomated == types.OptionalBoolTrue)
	}
	return true
}

func (f *SearchFilter) matchesOfficialFilter(result dockerTransport.SearchResult) bool {
	if f.IsOfficial != types.OptionalBoolUndefined {
		return result.IsOfficial == (f.IsOfficial == types.OptionalBoolTrue)
	}
	return true
}
