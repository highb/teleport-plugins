package main

import (
	"fmt"
	"strings"
	"time"
)

// Plugin data

type Resolution string

const Unresolved = Resolution("")
const ResolvedApproved = Resolution("approved")
const ResolvedDenied = Resolution("denied")
const ResolvedExpired = Resolution("expired")

type RequestData struct {
	User          string
	Roles         []string
	Created       time.Time
	RequestReason string
	ReviewsCount  int
	Resolution    Resolution
}

type PagerdutyData struct {
	ServiceID  string
	IncidentID string
}

type PluginData struct {
	RequestData
	PagerdutyData
}

// PagerDuty API types

type PaginationQuery struct {
	Limit  uint `url:"limit,omitempty"`
	Offset uint `url:"offset,omitempty"`
	Total  bool `url:"total,omitempty"`
}

type PaginationResult struct {
	Limit  uint `json:"limit"`
	Offset uint `json:"offset"`
	More   bool `json:"more"`
	Total  uint `json:"total"`
}

type ErrorResult struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Errors  []string `json:"errors"`
}

type Reference struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type,omitempty"`
}

type Details struct {
	Type    string `json:"type,omitempty"`
	Details string `json:"details,omitempty"`
}

type ExtensionSchema struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type ListExtensionSchemasResult struct {
	PaginationResult
	ExtensionSchemas []ExtensionSchema `json:"extension_schemas"`
}

type Extension struct {
	ID               string      `json:"id,omitempty"`
	Name             string      `json:"name"`
	EndpointURL      string      `json:"endpoint_url"`
	ExtensionObjects []Reference `json:"extension_objects"`
	ExtensionSchema  Reference   `json:"extension_schema"`
}

type ExtensionBody struct {
	Name             string      `json:"name"`
	EndpointURL      string      `json:"endpoint_url"`
	ExtensionObjects []Reference `json:"extension_objects"`
	ExtensionSchema  Reference   `json:"extension_schema"`
}

type ExtensionBodyWrap struct {
	Extension ExtensionBody `json:"extension"`
}

type ExtensionResult struct {
	Extension Extension `json:"extension"`
}

type ListExtensionsResult struct {
	PaginationResult
	Extensions []Extension `json:"extensions"`
}

type Service struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	EscalationPolicy Reference `json:"escalation_policy"`
}

type ServiceResult struct {
	Service Service `json:"service"`
}

type ListServicesQuery struct {
	PaginationQuery
	Query string `url:"query,omitempty"`
}

type ListServicesResult struct {
	PaginationResult
	Services []Service `json:"services"`
}

type Incident struct {
	ID          string               `json:"id"`
	Title       string               `json:"title"`
	Status      string               `json:"status"`
	IncidentKey string               `json:"incident_key"`
	Service     Reference            `json:"service"`
	Assignments []IncidentAssignment `json:"assignments"`
	Body        Details              `json:"body"`
}

type IncidentAssignment struct {
	At       string    `json:"at"`
	Assignee Reference `json:"assignee"`
}

type IncidentBody struct {
	ID          string    `json:"id,omitempty"`
	Title       string    `json:"title,omitempty"`
	IncidentKey string    `json:"incident_key,omitempty"`
	Service     Reference `json:"service,omitempty"`
	Body        Details   `json:"body,omitempty"`
	Type        string    `json:"type,omitempty"`
	Status      string    `json:"status,omitempty"`
}

type IncidentBodyWrap struct {
	Incident IncidentBody `json:"incident"`
}

type IncidentResult struct {
	Incident Incident `json:"incident"`
}

type ListIncidentsQuery struct {
	PaginationQuery
	UserIDs    []string `url:"user_ids,omitempty,brackets"`
	Statuses   []string `url:"statuses,omitempty,brackets"`
	ServiceIDs []string `url:"service_ids,omitempty,brackets"`
}

type ListIncidentsResult struct {
	PaginationResult
	Incidents []Incident `json:"incidents"`
}

type IncidentNote struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

type IncidentNoteBody struct {
	Content string `json:"content,omitempty"`
}

type IncidentNoteBodyWrap struct {
	Note IncidentNoteBody `json:"note"`
}

type IncidentNoteResult struct {
	Note IncidentNote `json:"note"`
}

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type UserResult struct {
	User User `json:"user"`
}

type ListUsersQuery struct {
	PaginationQuery
	Query string `url:"query,omitempty"`
}

type ListUsersResult struct {
	PaginationResult
	Users []User `json:"users"`
}

type OnCall struct {
	User             Reference `json:"user"`
	EscalationPolicy Reference `json:"escalation_policy"`
}

type ListOnCallsQuery struct {
	PaginationQuery
	UserIDs             []string `url:"user_ids,omitempty,brackets"`
	EscalationPolicyIDs []string `url:"escalation_policy_ids,omitempty,brackets"`
}

type ListOnCallsResult struct {
	PaginationResult
	OnCalls []OnCall `json:"oncalls"`
}

func DecodePluginData(dataMap map[string]string) (data PluginData) {
	var created int64
	data.User = dataMap["user"]
	data.Roles = strings.Split(dataMap["roles"], ",")
	fmt.Sscanf(dataMap["created"], "%d", &created)
	data.Created = time.Unix(created, 0)
	data.RequestReason = dataMap["request_reason"]
	if str := dataMap["reviews_count"]; str != "" {
		fmt.Sscanf(str, "%d", &data.ReviewsCount)
	}
	data.Resolution = Resolution(dataMap["resolution"])
	data.IncidentID = dataMap["incident_id"]
	data.ServiceID = dataMap["service_id"]
	return
}

func EncodePluginData(data PluginData) map[string]string {
	return map[string]string{
		"incident_id":    data.IncidentID,
		"service_id":     data.ServiceID,
		"user":           data.User,
		"roles":          strings.Join(data.Roles, ","),
		"created":        fmt.Sprintf("%d", data.Created.Unix()),
		"request_reason": data.RequestReason,
		"reviews_count":  fmt.Sprintf("%d", data.ReviewsCount),
		"resolution":     string(data.Resolution),
	}
}
