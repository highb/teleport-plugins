package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gravitational/teleport-plugins/lib"
	"github.com/gravitational/teleport-plugins/lib/logger"
	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"

	"github.com/gravitational/trace"
)

const (
	// minServerVersion is the minimal teleport version the plugin supports.
	minServerVersion = "6.1.0-beta.1"
	// pluginName is used to tag PluginData and as a Delegator in Audit log.
	pluginName = "pagerduty"
	// backoffMaxDelay is a maximum time GRPC client waits before reconnection attempt.
	backoffMaxDelay = time.Second * 2
	// initTimeout is used to bound execution time of health check and teleport version check.
	initTimeout = time.Second * 10
	// handlerTimeout is used to bound the execution time of watcher event handler.
	handlerTimeout = time.Second * 5
	// maxModifyPluginDataTries is a maximum number of compare-and-swap tries when modifying plugin data.
	maxModifyPluginDataTries = 5
)

// App contains global application state.
type App struct {
	conf Config

	apiClient *client.Client
	pagerduty Pagerduty
	mainJob   lib.ServiceJob

	*lib.Process
}

func NewApp(conf Config) (*App, error) {
	app := &App{conf: conf}
	app.mainJob = lib.NewServiceJob(app.run)
	return app, nil
}

// Run initializes and runs a watcher and a callback server
func (a *App) Run(ctx context.Context) error {
	// Initialize the process.
	a.Process = lib.NewProcess(ctx)
	a.SpawnCriticalJob(a.mainJob)
	<-a.Process.Done()
	return a.Err()
}

// Err returns the error app finished with.
func (a *App) Err() error {
	return trace.Wrap(a.mainJob.Err())
}

// WaitReady waits for http and watcher service to start up.
func (a *App) WaitReady(ctx context.Context) (bool, error) {
	return a.mainJob.WaitReady(ctx)
}

func (a *App) run(ctx context.Context) error {
	var err error

	logger.Get(ctx).Infof("Starting Teleport Access PagerDuty Plugin %s:%s", Version, Gitref)

	if err = a.init(ctx); err != nil {
		return trace.Wrap(err)
	}

	watcherJob := lib.NewWatcherJob(
		a.apiClient,
		lib.WatcherJobConfig{Watch: types.Watch{Kinds: []types.WatchKind{types.WatchKind{Kind: types.KindAccessRequest}}}},
		a.onWatcherEvent,
	)
	a.SpawnCriticalJob(watcherJob)
	watcherOk, err := watcherJob.WaitReady(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	a.mainJob.SetReady(watcherOk)

	<-watcherJob.Done()

	return trace.Wrap(watcherJob.Err())
}

func (a *App) init(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	log := logger.Get(ctx)

	var (
		err  error
		pong proto.PingResponse
	)

	bk := backoff.DefaultConfig
	bk.MaxDelay = backoffMaxDelay
	if a.apiClient, err = client.New(ctx, client.Config{
		Addrs:       []string{a.conf.Teleport.AuthServer},
		Credentials: a.conf.Teleport.Credentials(),
		DialOpts:    []grpc.DialOption{grpc.WithConnectParams(grpc.ConnectParams{Backoff: bk, MinConnectTimeout: initTimeout})},
	}); err != nil {
		return trace.Wrap(err)
	}

	if pong, err = a.checkTeleportVersion(ctx); err != nil {
		return trace.Wrap(err)
	}

	var webProxyAddr string
	if pong.ServerFeatures.AdvancedAccessWorkflows {
		webProxyAddr = pong.ProxyPublicAddr
	}
	a.pagerduty, err = NewPagerdutyClient(a.conf.Pagerduty, pong.ClusterName, webProxyAddr)
	if err != nil {
		return trace.Wrap(err)
	}

	log.Debug("Starting PagerDuty API health check...")
	if err = a.pagerduty.HealthCheck(ctx); err != nil {
		return trace.Wrap(err, "api health check failed. check your credentials and service_id settings")
	}
	log.Debug("PagerDuty API health check finished ok")

	return nil
}

func (a *App) checkTeleportVersion(ctx context.Context) (proto.PingResponse, error) {
	log := logger.Get(ctx)
	log.Debug("Checking Teleport server version")
	pong, err := a.apiClient.WithCallOptions(grpc.WaitForReady(true)).Ping(ctx)
	if err != nil {
		if trace.IsNotImplemented(err) {
			return pong, trace.Wrap(err, "server version must be at least %s", minServerVersion)
		}
		log.Error("Unable to get Teleport server version")
		return pong, trace.Wrap(err)
	}
	err = lib.AssertServerVersion(pong, minServerVersion)
	return pong, trace.Wrap(err)
}

func (a *App) onWatcherEvent(ctx context.Context, event types.Event) error {
	ctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()

	if kind := event.Resource.GetKind(); kind != types.KindAccessRequest {
		return trace.Errorf("unexpected kind %q", kind)
	}
	op := event.Type
	reqID := event.Resource.GetName()
	ctx, _ = logger.WithField(ctx, "request_id", reqID)

	switch op {
	case types.OpPut:
		ctx, _ = logger.WithField(ctx, "request_op", "put")
		req, ok := event.Resource.(types.AccessRequest)
		if !ok {
			return trace.Errorf("unexpected resource type %T", event.Resource)
		}
		ctx, log := logger.WithField(ctx, "request_state", req.GetState().String())

		var err error
		switch {
		case req.GetState().IsPending():
			err = a.onPendingRequest(ctx, req)
		case req.GetState().IsApproved():
			err = a.onResolvedRequest(ctx, req)
		case req.GetState().IsDenied():
			err = a.onResolvedRequest(ctx, req)
		default:
			log.WithField("event", event).Warn("Unknown request state")
			return nil
		}

		if err != nil {
			log.WithError(err).Error("Failed to process request")
			return err
		}

		return nil
	case types.OpDelete:
		ctx, log := logger.WithField(ctx, "request_op", "delete")

		if err := a.onDeletedRequest(ctx, reqID); err != nil {
			log.WithError(err).Error("Failed to process deleted request")
			return err
		}
		return nil
	default:
		return trace.BadParameter("unexpected event operation %s", op)
	}
}

func (a *App) onPendingRequest(ctx context.Context, req types.AccessRequest) error {
	if len(req.GetSystemAnnotations()) == 0 {
		logger.Get(ctx).Debug("Cannot proceed further. Request is missing any annotations")
		return nil
	}

	var (
		resultErr        error
		data             PagerdutyData
		shouldTryApprove bool
	)

	// First, try to create a notification incident.
	if serviceName, err := a.getNotifyServiceName(req); err == nil {
		if data, shouldTryApprove, err = a.tryNotifyService(ctx, req, serviceName); err != nil {
			resultErr = trace.Wrap(err)
			shouldTryApprove = true
		}
	} else {
		logger.Get(ctx).Debugf("Failed to determine a notification service info: %s", err.Error())
		shouldTryApprove = true
	}

	if !shouldTryApprove {
		return resultErr
	}

	// Then, try tro approve the request if user is currently on-call.
	err := a.tryAutoApproveRequest(ctx, req, data)
	return trace.NewAggregate(resultErr, trace.Wrap(err))
}

func (a *App) onResolvedRequest(ctx context.Context, req types.AccessRequest) error {
	var notifyErr error
	if _, err := a.tryNotifyReviews(ctx, req.GetName(), req.GetReviews()); err != nil {
		notifyErr = trace.Wrap(err)
	}

	var err error
	switch req.GetState() {
	case types.RequestState_APPROVED:
		err = trace.Wrap(a.tryResolveIncident(ctx, req.GetName(), req.GetResolveReason(), ResolvedApproved))
	case types.RequestState_DENIED:
		err = trace.Wrap(a.tryResolveIncident(ctx, req.GetName(), req.GetResolveReason(), ResolvedDenied))
	}
	return trace.NewAggregate(notifyErr, err)
}

func (a *App) onDeletedRequest(ctx context.Context, reqID string) error {
	return a.tryResolveIncident(ctx, reqID, "", ResolvedExpired)
}

func (a *App) getNotifyServiceName(req types.AccessRequest) (string, error) {
	annotationKey := a.conf.Pagerduty.RequestAnnotations.NotifyService
	slice, ok := req.GetSystemAnnotations()[annotationKey]
	if !ok {
		return "", trace.Errorf("request annotation %q is missing", annotationKey)
	}
	var serviceName string
	if len(slice) > 0 {
		serviceName = slice[0]
	}
	if serviceName == "" {
		return "", trace.Errorf("request annotation %q is empty", annotationKey)
	}
	return serviceName, nil
}

func (a *App) tryNotifyService(ctx context.Context, req types.AccessRequest, serviceName string) (PagerdutyData, bool, error) {
	ctx, _ = logger.WithField(ctx, "pd_service_name", serviceName)
	service, err := a.pagerduty.FindServiceByName(ctx, serviceName)
	if err != nil {
		return PagerdutyData{}, false, trace.Wrap(err)
	}

	reqID := req.GetName()
	reqData := RequestData{
		User:          req.GetUser(),
		Roles:         req.GetRoles(),
		Created:       req.GetCreationTime(),
		RequestReason: req.GetRequestReason(),
	}

	isNew, err := a.modifyPluginData(ctx, reqID, func(existing *PluginData) (PluginData, bool) {
		if existing != nil {
			return PluginData{}, false
		}
		return PluginData{RequestData: reqData}, true
	})
	if err != nil {
		return PagerdutyData{}, isNew, trace.Wrap(err)
	}

	var data PagerdutyData
	if isNew {
		if data, err = a.createIncident(ctx, service.ID, reqID, reqData); err != nil {
			return data, isNew, trace.Wrap(err)
		}
	}

	if reqReviews := req.GetReviews(); len(reqReviews) > 0 {
		if data, err = a.tryNotifyReviews(ctx, reqID, reqReviews); err != nil {
			return data, isNew, trace.Wrap(err)
		}
	}

	return data, isNew, nil
}

func (a *App) createIncident(ctx context.Context, serviceID, reqID string, reqData RequestData) (PagerdutyData, error) {
	data, err := a.pagerduty.CreateIncident(ctx, serviceID, reqID, reqData)
	if err != nil {
		return PagerdutyData{}, trace.Wrap(err)
	}
	ctx, log := logger.WithField(ctx, "pd_incident_id", data.IncidentID)
	log.Info("PagerDuty incident created")
	_, err = a.modifyPluginData(ctx, reqID, func(existing *PluginData) (PluginData, bool) {
		var pluginData PluginData
		if existing != nil {
			pluginData = *existing
		} else {
			// It must be an impossible case but lets handle it just in case.
			pluginData = PluginData{RequestData: reqData}
		}
		pluginData.PagerdutyData = data
		return pluginData, true
	})
	return data, trace.Wrap(err)
}

func (a *App) tryNotifyReviews(ctx context.Context, reqID string, reqReviews []types.AccessReview) (PagerdutyData, error) {
	var oldCount int
	var data PagerdutyData
	ok, err := a.modifyPluginData(ctx, reqID, func(existing *PluginData) (PluginData, bool) {
		if existing == nil {
			return PluginData{}, false
		}

		if data = existing.PagerdutyData; data.IncidentID == "" {
			return PluginData{}, false
		}

		count := len(reqReviews)
		if oldCount = existing.ReviewsCount; oldCount >= count {
			return PluginData{}, false
		}
		pluginData := *existing
		pluginData.ReviewsCount = count
		return pluginData, true
	})
	if err != nil || !ok {
		return data, trace.Wrap(err)
	}
	ctx, _ = logger.WithField(ctx, "pd_incident_id", data.IncidentID)

	slice := reqReviews[oldCount:]
	if len(slice) == 0 {
		return data, nil
	}

	errors := make([]error, 0, len(slice))
	for _, review := range slice {
		if err := a.pagerduty.PostReviewNote(ctx, data.IncidentID, review); err != nil {
			errors = append(errors, err)
		}
	}
	return data, trace.NewAggregate(errors...)
}

func (a *App) tryAutoApproveRequest(ctx context.Context, req types.AccessRequest, notificationData PagerdutyData) error {
	log := logger.Get(ctx)

	annotationKey := a.conf.Pagerduty.RequestAnnotations.ApprovalServices
	serviceNames, ok := req.GetSystemAnnotations()[annotationKey]
	if !ok {
		logger.Get(ctx).Debugf("Failed to auto-approve incident. Request annotation %q is missing", annotationKey)
		return nil
	}
	if len(serviceNames) == 0 {
		log.Warningf("Failed to find any service name. Request annotation %q is empty", annotationKey)
		return nil
	}

	userName := req.GetUser()
	if !lib.IsEmail(userName) {
		logger.Get(ctx).Warningf("Failed to auto-approve the request: %q does not look like a valid email", userName)
		return nil
	}

	user, err := a.pagerduty.FindUserByEmail(ctx, userName)
	if err != nil {
		if trace.IsNotFound(err) {
			log.WithError(err).Debug("Failed to auto-approve the request")
			return nil
		}
		return err
	}

	ctx, log = logger.WithFields(ctx, logger.Fields{
		"pd_user_email": user.Email,
		"pd_user_name":  user.Name,
	})

	services, err := a.pagerduty.FindServicesByNames(ctx, serviceNames)
	if err != nil {
		return trace.Wrap(err)
	}
	if len(services) == 0 {
		log.WithField("pd_service_names", serviceNames).Warning("Failed to find any service")
		return nil
	}

	if excludeID := notificationData.ServiceID; excludeID != "" {
		filteredServices := make([]Service, 0, len(services))
		for _, service := range services {
			if service.ID == excludeID {
				log.WithField("pd_service_name", service.Name).Warn("Notification service and approval services should not overlap")
				continue
			}
			filteredServices = append(filteredServices, service)
		}
		services = filteredServices
		if len(services) == 0 {
			return nil
		}
	}

	escalationPolicyMapping := make(map[string][]Service)
	for _, service := range services {
		escalationPolicyMapping[service.EscalationPolicy.ID] = append(escalationPolicyMapping[service.EscalationPolicy.ID], service)
	}
	var escalationPolicyIDs []string
	for id := range escalationPolicyMapping {
		escalationPolicyIDs = append(escalationPolicyIDs, id)
	}

	if escalationPolicyIDs, err = a.pagerduty.FilterOnCallPolicies(ctx, user.ID, escalationPolicyIDs); err != nil {
		return trace.Wrap(err)
	}
	if len(escalationPolicyIDs) == 0 {
		log.Debug("User is not on call")
		return nil
	}

	var serviceIDs []string
	for _, policyID := range escalationPolicyIDs {
		for _, service := range escalationPolicyMapping[policyID] {
			serviceIDs = append(serviceIDs, service.ID)
		}
	}

	if len(serviceIDs) == 0 {
		return nil
	}

	ok, err = a.pagerduty.HasAssignedIncidents(ctx, user.ID, serviceIDs)
	if err != nil {
		return trace.Wrap(err)
	}
	if !ok {
		log.Debug("User has no incidents assigned")
		return nil
	}

	if _, err := a.apiClient.SubmitAccessReview(ctx, types.AccessReviewSubmission{
		RequestID: req.GetName(),
		Review: types.AccessReview{
			ProposedState: types.RequestState_APPROVED,
			Reason:        fmt.Sprintf("Access requested by by on-call user %s (%s)", user.Name, user.Email),
			Created:       time.Now(),
		},
	}); err != nil {
		if strings.HasSuffix(err.Error(), "has already reviewed this request") {
			log.Debug("Already reviewed the request")
			return nil
		}
		return trace.Wrap(err)
	}

	log.Info("Successfully submitted a request auto-approve review")
	return nil
}

// resolveIncident resolves the notification incident created by plugin if the incident exists.
func (a *App) tryResolveIncident(ctx context.Context, reqID, reason string, resolution Resolution) error {
	var incidentID string
	ok, err := a.modifyPluginData(ctx, reqID, func(existing *PluginData) (PluginData, bool) {
		// If plugin data is empty or missing incidentID, we cannot do anything.
		if existing == nil {
			return PluginData{}, false
		}
		if incidentID = existing.IncidentID; incidentID == "" {
			return PluginData{}, false
		}

		// If resolution field is not empty then we already resolved the incident before. // In this case we just quit.
		if existing.RequestData.Resolution != Unresolved {
			return PluginData{}, false
		}

		// Mark incident as resolved.
		data := *existing
		data.Resolution = resolution
		return data, true
	})
	if err != nil {
		return trace.Wrap(err)
	}
	if !ok {
		return nil
	}

	ctx, log := logger.WithField(ctx, "pd_incident_id", incidentID)
	if err := a.pagerduty.ResolveIncident(ctx, incidentID, reason, resolution); err != nil {
		return trace.Wrap(err)
	}
	log.Info("Successfully resolved the incident")

	return nil
}

func (a *App) modifyPluginData(ctx context.Context, reqID string, fn func(data *PluginData) (PluginData, bool)) (bool, error) {
	var lastErr error
	for i := 0; i < maxModifyPluginDataTries; i++ {
		oldData, err := a.getPluginData(ctx, reqID)
		if err != nil {
			return false, trace.Wrap(err)
		}
		newData, ok := fn(oldData)
		if !ok {
			return false, nil
		}
		if oldData == nil {
			err = trace.Wrap(a.setNewPluginData(ctx, reqID, newData))
		} else {
			err = trace.Wrap(a.updatePluginData(ctx, reqID, newData, *oldData))
		}
		if err == nil {
			return true, nil
		}
		if trace.IsCompareFailed(err) {
			lastErr = err
			continue
		}
		return false, err
	}
	return false, lastErr
}

func (a *App) getPluginData(ctx context.Context, reqID string) (*PluginData, error) {
	dataMaps, err := a.apiClient.GetPluginData(ctx, types.PluginDataFilter{
		Kind:     types.KindAccessRequest,
		Resource: reqID,
		Plugin:   pluginName,
	})
	if err != nil {
		// TODO: handle trace.IsNotFound(err)
		return nil, trace.Wrap(err)
	}
	if len(dataMaps) == 0 {
		return nil, nil
	}
	entry := dataMaps[0].Entries()[pluginName]
	if entry == nil {
		return nil, nil
	}
	data := DecodePluginData(entry.Data)
	return &data, nil
}

// setNewPluginData sets plugin data but only if didn't exist before.
func (a *App) setNewPluginData(ctx context.Context, reqID string, data PluginData) error {
	expect := make(map[string]string)
	return a.apiClient.UpdatePluginData(ctx, types.PluginDataUpdateParams{
		Kind:     types.KindAccessRequest,
		Resource: reqID,
		Plugin:   pluginName,
		Set:      EncodePluginData(data),
		Expect:   expect,
	})
}

// updatePluginData updates an existing plugin data.
func (a *App) updatePluginData(ctx context.Context, reqID string, data PluginData, expectData PluginData) error {
	return a.apiClient.UpdatePluginData(ctx, types.PluginDataUpdateParams{
		Kind:     types.KindAccessRequest,
		Resource: reqID,
		Plugin:   pluginName,
		Set:      EncodePluginData(data),
		Expect:   EncodePluginData(expectData),
	})
}
