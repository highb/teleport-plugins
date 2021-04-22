package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/google/go-querystring/query"

	"github.com/gravitational/teleport-plugins/lib"
	"github.com/gravitational/teleport-plugins/lib/logger"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/trace"
)

const (
	pdMaxConns    = 100
	pdHTTPTimeout = 10 * time.Second
	pdListLimit   = uint(100)

	pdIncidentKeyPrefix = "teleport-access-request"
)

var incidentBodyTemplate *template.Template
var reviewNoteTemplate *template.Template
var resolutionNoteTemplate *template.Template

func init() {
	var err error

	incidentBodyTemplate, err = template.New("incident body").Parse(
		`{{.User}} requested permissions for roles {{range $index, $element := .Roles}}{{if $index}}, {{end}}{{ . }}{{end}} on Teleport at {{.Created.Format .TimeFormat}}.
{{if .RequestReason}}Reason: {{.RequestReason}}{{end}}
{{if .RequestLink}}To approve or deny the request, proceed to {{.RequestLink}}{{end}}
`,
	)
	if err != nil {
		panic(err)
	}

	reviewNoteTemplate, err = template.New("review note").Parse(
		`{{.Author}} reviewed the request at {{.Created.Format .TimeFormat}}.
Resolution: {{.ProposedState}}.
{{if .Reason}}Reason: {{.Reason}}.{{end}}`,
	)
	if err != nil {
		panic(err)
	}

	resolutionNoteTemplate, err = template.New("resolution note").Parse(
		`Access request has been {{.Resolution}}
{{if .ResolveReason}}Reason: {{.ResolveReason}}{{end}}`,
	)
	if err != nil {
		panic(err)
	}
}

// Pagerduty is a wrapper around resty.Client.
type Pagerduty struct {
	client *resty.Client
	from   string

	clusterName string
	webProxyURL *url.URL
}

func NewPagerdutyClient(conf PagerdutyConfig, clusterName, webProxyAddr string) (Pagerduty, error) {
	var webProxyURL *url.URL
	if webProxyAddr != "" {
		var err error
		if !strings.HasPrefix(webProxyAddr, "http://") && !strings.HasPrefix(webProxyAddr, "https://") {
			webProxyAddr = "https://" + webProxyAddr
		}
		if webProxyURL, err = url.Parse(webProxyAddr); err != nil {
			return Pagerduty{}, trace.Wrap(err)
		}
		if webProxyURL.Scheme == "https" && webProxyURL.Port() == "443" {
			// Cut off redundant :443
			webProxyURL.Host = webProxyURL.Hostname()
		}
	}

	client := resty.NewWithClient(&http.Client{
		Timeout: pdHTTPTimeout,
		Transport: &http.Transport{
			MaxConnsPerHost:     pdMaxConns,
			MaxIdleConnsPerHost: pdMaxConns,
		},
	})
	// APIEndpoint parameter is set only in tests
	if conf.APIEndpoint != "" {
		client.SetHostURL(conf.APIEndpoint)
	} else {
		client.SetHostURL("https://api.pagerduty.com")
	}
	client.SetHeader("Accept", "application/vnd.pagerduty+json;version=2")
	client.SetHeader("Content-Type", "application/json")
	client.SetHeader("Authorization", "Token token="+conf.APIKey)
	client.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {
		req.SetError(&ErrorResult{})
		return nil
	})
	client.OnAfterResponse(func(_ *resty.Client, resp *resty.Response) error {
		if resp.IsError() {
			result := resp.Error()
			if result, ok := result.(*ErrorResult); ok {
				return trace.Errorf("http error code=%v, err_code=%v, message=%v, errors=[%v]", resp.StatusCode(), result.Code, result.Message, strings.Join(result.Errors, ", "))
			}
			return trace.Errorf("unknown error result %#v", result)
		}
		return nil
	})
	return Pagerduty{
		client:      client,
		clusterName: clusterName,
		webProxyURL: webProxyURL,
		from:        conf.UserEmail,
	}, nil
}

func (p Pagerduty) HealthCheck(ctx context.Context) error {
	return nil
}

// CreateIncident creates a notification incident.
func (p Pagerduty) CreateIncident(ctx context.Context, serviceID, reqID string, reqData RequestData) (PagerdutyData, error) {
	bodyDetails, err := p.buildIncidentBody(reqID, reqData)
	if err != nil {
		return PagerdutyData{}, trace.Wrap(err)
	}
	body := IncidentBody{
		Title:       fmt.Sprintf("Access request from %s", reqData.User),
		IncidentKey: fmt.Sprintf("%s/%s", pdIncidentKeyPrefix, reqID),
		Service: Reference{
			Type: "service_reference",
			ID:   serviceID,
		},
		Body: Details{
			Type:    "incident_body",
			Details: bodyDetails,
		},
	}
	var result IncidentResult

	_, err = p.client.NewRequest().
		SetContext(ctx).
		SetHeader("From", p.from).
		SetBody(IncidentBodyWrap{body}).
		SetResult(&result).
		Post("incidents")
	if err != nil {
		return PagerdutyData{}, trace.Wrap(err)
	}

	return PagerdutyData{
		ServiceID:  serviceID,
		IncidentID: result.Incident.ID,
	}, nil
}

// PostReviewNote posts a note once a new request review appears.
func (p Pagerduty) PostReviewNote(ctx context.Context, incidentID string, review types.AccessReview) error {
	noteContent, err := p.buildReviewNoteBody(review)
	if err != nil {
		return trace.Wrap(err)
	}

	_, err = p.client.NewRequest().
		SetContext(ctx).
		SetHeader("From", p.from).
		SetBody(IncidentNoteBodyWrap{IncidentNoteBody{Content: noteContent}}).
		SetPathParams(map[string]string{"incidentID": incidentID}).
		Post("incidents/{incidentID}/notes")
	return trace.Wrap(err)
}

// ResolveIncident resolves an incident and posts a note with resolution details.
func (p Pagerduty) ResolveIncident(ctx context.Context, incidentID, reason string, resolution Resolution) error {
	noteContent, err := p.buildResolutionNoteBody(reason, resolution)
	if err != nil {
		return trace.Wrap(err)
	}

	pathParams := map[string]string{"incidentID": incidentID}

	_, err = p.client.NewRequest().
		SetContext(ctx).
		SetHeader("From", p.from).
		SetBody(IncidentNoteBodyWrap{IncidentNoteBody{Content: noteContent}}).
		SetPathParams(pathParams).
		Post("incidents/{incidentID}/notes")
	if err != nil {
		return trace.Wrap(err)
	}

	_, err = p.client.NewRequest().
		SetContext(ctx).
		SetHeader("From", p.from).
		SetBody(IncidentBodyWrap{IncidentBody{
			Type:   "incident_reference",
			Status: "resolved",
		}}).
		SetPathParams(pathParams).
		Put("incidents/{incidentID}")
	return trace.Wrap(err)
}

// GetUserInfo loads a user profile by id.
func (p Pagerduty) GetUserInfo(ctx context.Context, userID string) (User, error) {
	var result UserResult

	_, err := p.client.NewRequest().
		SetContext(ctx).
		SetResult(&result).
		SetPathParams(map[string]string{"userID": userID}).
		Get("users/{userID}")
	if err != nil {
		return User{}, trace.Wrap(err)
	}

	return result.User, nil
}

// GetUserByEmail finds a user by email.
func (p Pagerduty) FindUserByEmail(ctx context.Context, userEmail string) (User, error) {
	userEmail = strings.ToLower(userEmail)
	usersQuery, err := query.Values(ListUsersQuery{
		Query: userEmail,
		PaginationQuery: PaginationQuery{
			Limit: pdListLimit,
		},
	})
	if err != nil {
		return User{}, trace.Wrap(err)
	}

	var result ListUsersResult
	_, err = p.client.NewRequest().
		SetContext(ctx).
		SetQueryParamsFromValues(usersQuery).
		SetResult(&result).
		Get("users")
	if err != nil {
		return User{}, trace.Wrap(err)
	}

	for _, user := range result.Users {
		if strings.ToLower(user.Email) == userEmail {
			return user, nil
		}
	}

	if len(result.Users) > 0 && result.More {
		logger.Get(ctx).Warningf("PagerDuty returned too many results when querying by email %q", userEmail)
	}

	return User{}, trace.NotFound("failed to find pagerduty user by email %q", userEmail)
}

// FindServiceByName finds a service by its name (case-insensitive).
func (p Pagerduty) FindServiceByName(ctx context.Context, serviceName string) (Service, error) {
	// In PagerDuty service names are unique and in fact case-insensitive.
	serviceName = strings.ToLower(serviceName)
	servicesQuery, err := query.Values(ListServicesQuery{Query: serviceName})
	if err != nil {
		return Service{}, trace.Wrap(err)
	}
	var result ListServicesResult
	_, err = p.client.NewRequest().
		SetContext(ctx).
		SetQueryParamsFromValues(servicesQuery).
		SetResult(&result).
		Get("services")
	if err != nil {
		return Service{}, trace.Wrap(err)
	}

	for _, service := range result.Services {
		if strings.ToLower(service.Name) == serviceName {
			return service, nil
		}
	}

	return Service{}, trace.NotFound("failed to find pagerduty service by name %q", serviceName)
}

// FindServicesByNames finds a bunch of services by its names making a query for each service.
func (p Pagerduty) FindServicesByNames(ctx context.Context, serviceNames []string) ([]Service, error) {
	services := make([]Service, 0, len(serviceNames))
	for _, serviceName := range serviceNames {
		service, err := p.FindServiceByName(ctx, serviceName)
		if err != nil {
			if trace.IsNotFound(err) {
				continue
			}
			return nil, trace.Wrap(err)
		}
		services = append(services, service)
	}
	return services, nil
}

// RangeOnCallPolicies iterates over the escalation policy IDs for which a given user is currently on-call.
func (p Pagerduty) FilterOnCallPolicies(ctx context.Context, userID string, escalationPolicyIDs []string) ([]string, error) {
	policyIDSet := make(map[string]struct{})
	for _, id := range escalationPolicyIDs {
		policyIDSet[id] = struct{}{}
	}

	filteredIDSet := make(map[string]struct{})

	var offset uint
	more := true
	anyData := false
	for more {
		query, err := query.Values(ListOnCallsQuery{
			PaginationQuery:     PaginationQuery{Offset: offset},
			UserIDs:             []string{userID},
			EscalationPolicyIDs: escalationPolicyIDs,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		var result ListOnCallsResult

		_, err = p.client.NewRequest().
			SetContext(ctx).
			SetQueryParamsFromValues(query).
			SetResult(&result).
			Get("oncalls")
		if err != nil {
			return nil, trace.Wrap(err)
		}
		more = result.More
		offset += uint(len(result.OnCalls))
		anyData = anyData || len(result.OnCalls) > 0

		for _, onCall := range result.OnCalls {
			if !(onCall.User.Type == "user_reference" && onCall.User.ID == userID) {
				continue
			}

			id := onCall.EscalationPolicy.ID
			if _, ok := policyIDSet[id]; ok {
				filteredIDSet[id] = struct{}{}
			}
		}

		if len(filteredIDSet) == len(policyIDSet) {
			more = false
		}
	}

	if len(filteredIDSet) == 0 {
		if anyData {
			logger.Get(ctx).WithFields(logger.Fields{
				"pd_user_id":               userID,
				"pd_escalation_policy_ids": escalationPolicyIDs,
			}).Warningf("PagerDuty returned some oncalls array but none of them matched the query")
		}

		return nil, nil
	}

	result := make([]string, 0, len(filteredIDSet))
	for id := range filteredIDSet {
		result = append(result, id)
	}
	return result, nil
}

// HasAssignedIncidents determines if user has any incidents assigned in a give set of services.
func (p Pagerduty) HasAssignedIncidents(ctx context.Context, userID string, serviceIDs []string) (bool, error) {
	query, err := query.Values(ListIncidentsQuery{
		PaginationQuery: PaginationQuery{Limit: 1},
		UserIDs:         []string{userID},
		ServiceIDs:      serviceIDs,
	})
	if err != nil {
		return false, trace.Wrap(err)
	}
	var result ListIncidentsResult
	_, err = p.client.NewRequest().
		SetContext(ctx).
		SetQueryParamsFromValues(query).
		SetResult(&result).
		Get("incidents")
	if err != nil {
		return false, trace.Wrap(err)
	}

	for _, incident := range result.Incidents {
		var userOk bool
		for _, assignment := range incident.Assignments {
			if assignment.Assignee.Type == "user_reference" && assignment.Assignee.ID == userID {
				userOk = true
				break
			}
		}
		if !userOk {
			continue
		}
		for _, id := range serviceIDs {
			if incident.Service.Type == "service_reference" && incident.Service.ID == id {
				return true, nil
			}
		}
	}

	return false, nil
}

func (p Pagerduty) buildIncidentBody(reqID string, reqData RequestData) (string, error) {
	var requestLink string
	if p.webProxyURL != nil {
		reqURL := *p.webProxyURL
		reqURL.Path = lib.BuildURLPath("web", "requests", reqID)
		requestLink = reqURL.String()
	}

	var builder strings.Builder
	err := incidentBodyTemplate.Execute(&builder, struct {
		ID          string
		TimeFormat  string
		RequestLink string
		RequestData
	}{
		reqID,
		time.RFC822,
		requestLink,
		reqData,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return builder.String(), nil
}

func (p Pagerduty) buildReviewNoteBody(review types.AccessReview) (string, error) {
	var builder strings.Builder
	err := reviewNoteTemplate.Execute(&builder, struct {
		types.AccessReview
		ProposedState string
		TimeFormat    string
	}{
		review,
		review.ProposedState.String(),
		time.RFC822,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return builder.String(), nil
}

func (p Pagerduty) buildResolutionNoteBody(reason string, resolution Resolution) (string, error) {
	var builder strings.Builder
	err := resolutionNoteTemplate.Execute(&builder, struct {
		Resolution    string
		ResolveReason string
	}{
		string(resolution),
		reason,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return builder.String(), nil
}
