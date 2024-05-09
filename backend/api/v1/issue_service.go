package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bytebase/bytebase/backend/common"
	"github.com/bytebase/bytebase/backend/common/log"
	"github.com/bytebase/bytebase/backend/component/activity"
	"github.com/bytebase/bytebase/backend/component/config"
	"github.com/bytebase/bytebase/backend/component/iam"
	"github.com/bytebase/bytebase/backend/component/state"
	enterprise "github.com/bytebase/bytebase/backend/enterprise/api"
	api "github.com/bytebase/bytebase/backend/legacyapi"
	metricapi "github.com/bytebase/bytebase/backend/metric"
	relayplugin "github.com/bytebase/bytebase/backend/plugin/app/relay"
	"github.com/bytebase/bytebase/backend/plugin/metric"
	"github.com/bytebase/bytebase/backend/runner/metricreport"
	"github.com/bytebase/bytebase/backend/runner/relay"
	"github.com/bytebase/bytebase/backend/store"
	"github.com/bytebase/bytebase/backend/utils"
	storepb "github.com/bytebase/bytebase/proto/generated-go/store"
	v1pb "github.com/bytebase/bytebase/proto/generated-go/v1"
)

// IssueService implements the issue service.
type IssueService struct {
	v1pb.UnimplementedIssueServiceServer
	store           *store.Store
	activityManager *activity.Manager
	relayRunner     *relay.Runner
	stateCfg        *state.State
	licenseService  enterprise.LicenseService
	profile         *config.Profile
	iamManager      *iam.Manager
	metricReporter  *metricreport.Reporter
}

// NewIssueService creates a new IssueService.
func NewIssueService(
	store *store.Store,
	activityManager *activity.Manager,
	relayRunner *relay.Runner,
	stateCfg *state.State,
	licenseService enterprise.LicenseService,
	profile *config.Profile,
	iamManager *iam.Manager,
	metricReporter *metricreport.Reporter,
) *IssueService {
	return &IssueService{
		store:           store,
		activityManager: activityManager,
		relayRunner:     relayRunner,
		stateCfg:        stateCfg,
		licenseService:  licenseService,
		profile:         profile,
		iamManager:      iamManager,
		metricReporter:  metricReporter,
	}
}

// GetIssue gets a issue.
func (s *IssueService) GetIssue(ctx context.Context, request *v1pb.GetIssueRequest) (*v1pb.Issue, error) {
	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}
	issue, err := s.getIssueMessage(ctx, request.Name)
	if err != nil {
		return nil, err
	}
	if request.Force {
		externalApprovalType := api.ExternalApprovalTypeRelay
		approvals, err := s.store.ListExternalApprovalV2(ctx, &store.ListExternalApprovalMessage{
			Type:     &externalApprovalType,
			IssueUID: &issue.UID,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to list external approvals, error: %v", err)
		}
		var errs error
		for _, approval := range approvals {
			msg := relay.CheckExternalApprovalChanMessage{
				ExternalApproval: approval,
				ErrChan:          make(chan error, 1),
			}
			s.relayRunner.CheckExternalApprovalChan <- msg
			err := <-msg.ErrChan
			if err != nil {
				err = errors.Wrapf(err, "failed to check external approval status, issueUID %d", approval.IssueUID)
				errs = multierr.Append(errs, err)
			}
		}
		if errs != nil {
			return nil, status.Errorf(codes.Internal, "failed to check external approval status, error: %v", errs)
		}
		issue, err = s.getIssueMessage(ctx, request.Name)
		if err != nil {
			return nil, err
		}
	}

	if err := s.canUserGetIssue(ctx, issue, user); err != nil {
		return nil, err
	}

	issueV1, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return issueV1, nil
}

func (s *IssueService) canUserGetIssue(ctx context.Context, issue *store.IssueMessage, user *store.UserMessage) error {
	// allow creator to get issue.
	if issue.Creator.ID == user.ID {
		return nil
	}
	needPermissions := []iam.Permission{iam.PermissionIssuesGet}
	if issue.Type == api.IssueDatabaseGeneral || issue.Type == api.IssueDatabaseDataExport {
		needPermissions = append(needPermissions, iam.PermissionPlansGet)
	}
	for _, p := range needPermissions {
		ok, err := s.iamManager.CheckPermission(ctx, p, user, issue.Project.ResourceID)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to check permission, error: %v", err)
		}
		if !ok {
			return status.Errorf(codes.PermissionDenied, "permission denied to get issue, user does not have permission %q", p)
		}
	}
	return nil
}

func (s *IssueService) getIssueFind(ctx context.Context, permissionFilter *store.FindIssueMessagePermissionFilter, projectID string, filter string, query string, limit, offset *int) (*store.FindIssueMessage, error) {
	issueFind := &store.FindIssueMessage{
		PermissionFilter: permissionFilter,
		Limit:            limit,
		Offset:           offset,
	}
	if projectID != "-" {
		issueFind.ProjectID = &projectID
	}
	if query != "" {
		issueFind.Query = &query
	}
	filters, err := parseFilter(filter)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	for _, spec := range filters {
		switch spec.key {
		case "creator":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "creator" filter`)
			}
			user, err := s.getUserByIdentifier(ctx, spec.value)
			if err != nil {
				return nil, err
			}
			issueFind.CreatorID = &user.ID
		case "assignee":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "assignee" filter`)
			}
			user, err := s.getUserByIdentifier(ctx, spec.value)
			if err != nil {
				return nil, err
			}
			issueFind.AssigneeID = &user.ID
		case "subscriber":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "subscriber" filter`)
			}
			user, err := s.getUserByIdentifier(ctx, spec.value)
			if err != nil {
				return nil, err
			}
			issueFind.SubscriberID = &user.ID
		case "status":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "status" filter`)
			}
			for _, raw := range strings.Split(spec.value, " | ") {
				newStatus, err := convertToAPIIssueStatus(v1pb.IssueStatus(v1pb.IssueStatus_value[raw]))
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "failed to convert to issue status, err: %v", err)
				}
				issueFind.StatusList = append(issueFind.StatusList, newStatus)
			}
		case "create_time":
			if spec.operator != comparatorTypeGreaterEqual && spec.operator != comparatorTypeLessEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "<=" or ">=" operation for "create_time" filter`)
			}
			t, err := time.Parse(time.RFC3339, spec.value)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "failed to parse create_time %s, err: %v", spec.value, err)
			}
			ts := t.Unix()
			if spec.operator == comparatorTypeGreaterEqual {
				issueFind.CreatedTsAfter = &ts
			} else {
				issueFind.CreatedTsBefore = &ts
			}
		case "create_time_after":
			t, err := time.Parse(time.RFC3339, spec.value)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "failed to parse create_time_after %s, err: %v", spec.value, err)
			}
			ts := t.Unix()
			issueFind.CreatedTsAfter = &ts
		case "type":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "type" filter`)
			}
			switch spec.value {
			case "DDL":
				issueFind.TaskTypes = &[]api.TaskType{
					api.TaskDatabaseSchemaUpdate,
					api.TaskDatabaseSchemaUpdateSDL,
					api.TaskDatabaseSchemaUpdateGhostSync,
					api.TaskDatabaseSchemaUpdateGhostCutover,
				}
			case "DML":
				issueFind.TaskTypes = &[]api.TaskType{
					api.TaskDatabaseDataUpdate,
				}
			case "DATA_EXPORT":
				issueFind.TaskTypes = &[]api.TaskType{
					api.TaskDatabaseDataExport,
				}
			default:
				return nil, status.Errorf(codes.InvalidArgument, `unknown value %q`, spec.value)
			}
		case "instance":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "instance" filter`)
			}
			instanceResourceID, err := common.GetInstanceID(spec.value)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, `invalid instance resource id "%s": %v`, spec.value, err.Error())
			}
			issueFind.InstanceResourceID = &instanceResourceID
		case "database":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "database" filter`)
			}
			instanceID, databaseName, err := common.GetInstanceDatabaseID(spec.value)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, err.Error())
			}
			database, err := s.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
				InstanceID:   &instanceID,
				DatabaseName: &databaseName,
			})
			if err != nil {
				return nil, status.Errorf(codes.Internal, err.Error())
			}
			if database == nil {
				return nil, status.Errorf(codes.InvalidArgument, `database "%q" not found`, spec.value)
			}
			issueFind.DatabaseUID = &database.UID
		case "labels":
			if spec.operator != comparatorTypeEqual {
				return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "%s" filter`, spec.key)
			}
			for _, label := range strings.Split(spec.value, " & ") {
				issueLabel := label
				issueFind.LabelList = append(issueFind.LabelList, issueLabel)
			}
		}
	}

	return issueFind, nil
}

func (s *IssueService) ListIssues(ctx context.Context, request *v1pb.ListIssuesRequest) (*v1pb.ListIssuesResponse, error) {
	if request.PageSize < 0 {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("page size must be non-negative: %d", request.PageSize))
	}

	requestProjectID, err := common.GetProjectID(request.Parent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}
	permissionFilter, err := func() (*store.FindIssueMessagePermissionFilter, error) {
		return getIssuePermissionFilter(ctx, s.store, user, s.iamManager, iam.PermissionIssuesList)
	}()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get project ids and issue types filter, error: %v", err)
	}

	limit, offset, err := parseLimitAndOffset(request.PageToken, int(request.PageSize))
	if err != nil {
		return nil, err
	}
	limitPlusOne := limit + 1

	issueFind, err := s.getIssueFind(ctx, permissionFilter, requestProjectID, request.Filter, request.Query, &limitPlusOne, &offset)
	if err != nil {
		return nil, err
	}

	issues, err := s.store.ListIssueV2(ctx, issueFind)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to search issue, error: %v", err)
	}

	var nextPageToken string
	if len(issues) == limitPlusOne {
		pageToken, err := getPageToken(limit, offset+limit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get next page token, error: %v", err)
		}
		nextPageToken = pageToken
		issues = issues[:limit]
	}

	converted, err := convertToIssues(ctx, s.store, issues)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return &v1pb.ListIssuesResponse{
		Issues:        converted,
		NextPageToken: nextPageToken,
	}, nil
}

func (s *IssueService) SearchIssues(ctx context.Context, request *v1pb.SearchIssuesRequest) (*v1pb.SearchIssuesResponse, error) {
	if request.PageSize < 0 {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("page size must be non-negative: %d", request.PageSize))
	}

	requestProjectID, err := common.GetProjectID(request.Parent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}

	permissionFilter, err := getIssuePermissionFilter(ctx, s.store, user, s.iamManager, iam.PermissionIssuesList)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get project ids and issue types filter, error: %v", err)
	}

	limit, offset, err := parseLimitAndOffset(request.PageToken, int(request.PageSize))
	if err != nil {
		return nil, err
	}
	limitPlusOne := limit + 1

	issueFind, err := s.getIssueFind(ctx, permissionFilter, requestProjectID, request.Filter, request.Query, &limitPlusOne, &offset)
	if err != nil {
		return nil, err
	}

	issues, err := s.store.ListIssueV2(ctx, issueFind)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to search issue, error: %v", err)
	}

	if len(issues) == limitPlusOne {
		nextPageToken, err := getPageToken(limit, offset+limit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get next page token, error: %v", err)
		}
		converted, err := convertToIssues(ctx, s.store, issues[:limit])
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
		}
		return &v1pb.SearchIssuesResponse{
			Issues:        converted,
			NextPageToken: nextPageToken,
		}, nil
	}

	// No subsequent pages.
	converted, err := convertToIssues(ctx, s.store, issues)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return &v1pb.SearchIssuesResponse{
		Issues:        converted,
		NextPageToken: "",
	}, nil
}

func (s *IssueService) getUserByIdentifier(ctx context.Context, identifier string) (*store.UserMessage, error) {
	email := strings.TrimPrefix(identifier, "users/")
	if email == "" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid empty creator identifier")
	}
	user, err := s.store.GetUser(ctx, &store.FindUserMessage{
		Email:       &email,
		ShowDeleted: true,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, `failed to find user "%s" with error: %v`, email, err.Error())
	}
	if user == nil {
		return nil, errors.Errorf("cannot found user %s", email)
	}
	return user, nil
}

// CreateIssue creates a issue.
func (s *IssueService) CreateIssue(ctx context.Context, request *v1pb.CreateIssueRequest) (*v1pb.Issue, error) {
	projectID, err := common.GetProjectID(request.Parent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	_, loopback := ctx.Value(common.LoopbackContextKey).(bool)

	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}
	if !loopback {
		ok, err := s.iamManager.CheckPermission(ctx, iam.PermissionIssuesCreate, user, projectID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to check permission, error: %v", err)
		}
		if !ok {
			return nil, status.Errorf(codes.PermissionDenied, "permission denied to create issue")
		}
	}

	switch request.Issue.Type {
	case v1pb.Issue_TYPE_UNSPECIFIED:
		return nil, status.Errorf(codes.InvalidArgument, "issue type is required")
	case v1pb.Issue_GRANT_REQUEST:
		return s.createIssueGrantRequest(ctx, request)
	case v1pb.Issue_DATABASE_CHANGE:
		return s.createIssueDatabaseChange(ctx, request)
	case v1pb.Issue_DATABASE_DATA_EXPORT:
		return s.createIssueDatabaseDataExport(ctx, request)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown issue type %q", request.Issue.Type)
	}
}

func (s *IssueService) createIssueDatabaseChange(ctx context.Context, request *v1pb.CreateIssueRequest) (*v1pb.Issue, error) {
	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}
	projectID, err := common.GetProjectID(request.Parent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	project, err := s.store.GetProjectV2(ctx, &store.FindProjectMessage{
		ResourceID: &projectID,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get project, error: %v", err)
	}
	if project == nil {
		return nil, status.Errorf(codes.NotFound, "project not found for id: %v", projectID)
	}

	if request.Issue.Plan == "" {
		return nil, status.Errorf(codes.InvalidArgument, "plan is required")
	}

	var planUID *int64
	planID, err := common.GetPlanID(request.Issue.Plan)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	plan, err := s.store.GetPlan(ctx, &store.FindPlanMessage{UID: &planID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get plan, error: %v", err)
	}
	if plan == nil {
		return nil, status.Errorf(codes.NotFound, "plan not found for id: %d", planID)
	}
	planUID = &plan.UID
	var rolloutUID *int
	if request.Issue.Rollout != "" {
		_, rolloutID, err := common.GetProjectIDRolloutID(request.Issue.Rollout)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, err.Error())
		}
		pipeline, err := s.store.GetPipelineV2ByID(ctx, rolloutID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get rollout, error: %v", err)
		}
		if pipeline == nil {
			return nil, status.Errorf(codes.NotFound, "rollout not found for id: %d", rolloutID)
		}
		rolloutUID = &pipeline.ID
	}

	var issueAssignee *store.UserMessage
	if request.Issue.Assignee != "" {
		assigneeEmail, err := common.GetUserEmail(request.Issue.Assignee)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, err.Error())
		}
		assignee, err := s.store.GetUser(ctx, &store.FindUserMessage{Email: &assigneeEmail})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get user by email %q, error: %v", assigneeEmail, err)
		}
		if assignee == nil {
			return nil, status.Errorf(codes.NotFound, "assignee not found for email: %q", assigneeEmail)
		}
		issueAssignee = assignee
	}

	issueCreateMessage := &store.IssueMessage{
		Project:     project,
		PlanUID:     planUID,
		PipelineUID: rolloutUID,
		Title:       request.Issue.Title,
		Status:      api.IssueOpen,
		Type:        api.IssueDatabaseGeneral,
		Description: request.Issue.Description,
		Assignee:    issueAssignee,
	}

	issueCreateMessage.Payload = &storepb.IssuePayload{
		Approval: &storepb.IssuePayloadApproval{
			ApprovalFindingDone: false,
			ApprovalTemplates:   nil,
			Approvers:           nil,
		},
		Labels: request.Issue.Labels,
	}

	issue, err := s.store.CreateIssueV2(ctx, issueCreateMessage, principalID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create issue, error: %v", err)
	}
	s.stateCfg.ApprovalFinding.Store(issue.UID, issue)

	createActivityPayload := api.ActivityIssueCreatePayload{
		IssueName: issue.Title,
	}
	bytes, err := json.Marshal(createActivityPayload)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create ActivityIssueCreate activity after creating the issue: %v", issue.Title)
	}
	activityCreate := &store.ActivityMessage{
		CreatorUID:        principalID,
		ResourceContainer: project.GetName(),
		ContainerUID:      issue.UID,
		Type:              api.ActivityIssueCreate,
		Level:             api.ActivityInfo,
		Payload:           string(bytes),
	}
	if _, err := s.activityManager.CreateActivity(ctx, activityCreate, &activity.Metadata{
		Issue: issue,
	}); err != nil {
		return nil, errors.Wrapf(err, "failed to create ActivityIssueCreate activity after creating the issue: %v", issue.Title)
	}

	converted, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}

	return converted, nil
}

func (s *IssueService) createIssueGrantRequest(ctx context.Context, request *v1pb.CreateIssueRequest) (*v1pb.Issue, error) {
	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}
	projectID, err := common.GetProjectID(request.Parent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	project, err := s.store.GetProjectV2(ctx, &store.FindProjectMessage{
		ResourceID: &projectID,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get project, error: %v", err)
	}
	if project == nil {
		return nil, status.Errorf(codes.NotFound, "project not found for id: %v", projectID)
	}

	if request.Issue.GrantRequest.GetRole() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "expect grant request role")
	}
	if request.Issue.GrantRequest.GetUser() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "expect grant request user")
	}
	// Validate CEL expression if it's not empty.
	if expression := request.Issue.GrantRequest.GetCondition().GetExpression(); expression != "" {
		e, err := cel.NewEnv(common.IAMPolicyConditionCELAttributes...)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to create cel environment, error: %v", err)
		}
		if _, issues := e.Compile(expression); issues != nil {
			return nil, status.Errorf(codes.InvalidArgument, "found issues in grant request condition expression, issues: %v", issues.String())
		}
	}

	issueCreateMessage := &store.IssueMessage{
		Project:     project,
		PlanUID:     nil,
		PipelineUID: nil,
		Title:       request.Issue.Title,
		Status:      api.IssueOpen,
		Type:        api.IssueGrantRequest,
		Description: request.Issue.Description,
		Assignee:    nil,
	}

	convertedGrantRequest, err := convertGrantRequest(ctx, s.store, request.Issue.GrantRequest)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert GrantRequest, error: %v", err)
	}

	issueCreateMessage.Payload = &storepb.IssuePayload{
		GrantRequest: convertedGrantRequest,
		Approval: &storepb.IssuePayloadApproval{
			ApprovalFindingDone: false,
			ApprovalTemplates:   nil,
			Approvers:           nil,
		},
		Labels: request.Issue.Labels,
	}

	issue, err := s.store.CreateIssueV2(ctx, issueCreateMessage, principalID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create issue, error: %v", err)
	}
	s.stateCfg.ApprovalFinding.Store(issue.UID, issue)

	createActivityPayload := api.ActivityIssueCreatePayload{
		IssueName: issue.Title,
	}
	bytes, err := json.Marshal(createActivityPayload)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create ActivityIssueCreate activity after creating the issue: %v", issue.Title)
	}
	activityCreate := &store.ActivityMessage{
		CreatorUID:        principalID,
		ResourceContainer: project.GetName(),
		ContainerUID:      issue.UID,
		Type:              api.ActivityIssueCreate,
		Level:             api.ActivityInfo,
		Payload:           string(bytes),
	}
	if _, err := s.activityManager.CreateActivity(ctx, activityCreate, &activity.Metadata{
		Issue: issue,
	}); err != nil {
		return nil, errors.Wrapf(err, "failed to create ActivityIssueCreate activity after creating the issue: %v", issue.Title)
	}

	converted, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}

	s.metricReporter.Report(ctx, &metric.Metric{
		Name:  metricapi.IssueCreateMetricName,
		Value: 1,
		Labels: map[string]any{
			"type": issue.Type,
		},
	})

	return converted, nil
}

func (s *IssueService) createIssueDatabaseDataExport(ctx context.Context, request *v1pb.CreateIssueRequest) (*v1pb.Issue, error) {
	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}
	projectID, err := common.GetProjectID(request.Parent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	project, err := s.store.GetProjectV2(ctx, &store.FindProjectMessage{
		ResourceID: &projectID,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get project, error: %v", err)
	}
	if project == nil {
		return nil, status.Errorf(codes.NotFound, "project not found for id: %v", projectID)
	}

	if request.Issue.Plan == "" {
		return nil, status.Errorf(codes.InvalidArgument, "plan is required")
	}

	var planUID *int64
	planID, err := common.GetPlanID(request.Issue.Plan)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	plan, err := s.store.GetPlan(ctx, &store.FindPlanMessage{UID: &planID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get plan, error: %v", err)
	}
	if plan == nil {
		return nil, status.Errorf(codes.NotFound, "plan not found for id: %d", planID)
	}
	planUID = &plan.UID
	var rolloutUID *int
	if request.Issue.Rollout != "" {
		_, rolloutID, err := common.GetProjectIDRolloutID(request.Issue.Rollout)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, err.Error())
		}
		pipeline, err := s.store.GetPipelineV2ByID(ctx, rolloutID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get rollout, error: %v", err)
		}
		if pipeline == nil {
			return nil, status.Errorf(codes.NotFound, "rollout not found for id: %d", rolloutID)
		}
		rolloutUID = &pipeline.ID
	}

	var issueAssignee *store.UserMessage
	if request.Issue.Assignee != "" {
		assigneeEmail, err := common.GetUserEmail(request.Issue.Assignee)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, err.Error())
		}
		assignee, err := s.store.GetUser(ctx, &store.FindUserMessage{Email: &assigneeEmail})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get user by email %q, error: %v", assigneeEmail, err)
		}
		if assignee == nil {
			return nil, status.Errorf(codes.NotFound, "assignee not found for email: %q", assigneeEmail)
		}
		issueAssignee = assignee
	}

	issueCreateMessage := &store.IssueMessage{
		Project:     project,
		PlanUID:     planUID,
		PipelineUID: rolloutUID,
		Title:       request.Issue.Title,
		Status:      api.IssueOpen,
		Type:        api.IssueDatabaseDataExport,
		Description: request.Issue.Description,
		Assignee:    issueAssignee,
	}

	issueCreateMessage.Payload = &storepb.IssuePayload{
		Approval: &storepb.IssuePayloadApproval{
			ApprovalFindingDone: false,
			ApprovalTemplates:   nil,
			Approvers:           nil,
		},
		Labels: request.Issue.Labels,
	}

	issue, err := s.store.CreateIssueV2(ctx, issueCreateMessage, principalID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create issue, error: %v", err)
	}
	s.stateCfg.ApprovalFinding.Store(issue.UID, issue)

	createActivityPayload := api.ActivityIssueCreatePayload{
		IssueName: issue.Title,
	}
	bytes, err := json.Marshal(createActivityPayload)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create ActivityIssueCreate activity after creating the issue: %v", issue.Title)
	}
	activityCreate := &store.ActivityMessage{
		CreatorUID:        principalID,
		ResourceContainer: project.GetName(),
		ContainerUID:      issue.UID,
		Type:              api.ActivityIssueCreate,
		Level:             api.ActivityInfo,
		Payload:           string(bytes),
	}
	if _, err := s.activityManager.CreateActivity(ctx, activityCreate, &activity.Metadata{
		Issue: issue,
	}); err != nil {
		return nil, errors.Wrapf(err, "failed to create ActivityIssueCreate activity after creating the issue: %v", issue.Title)
	}

	converted, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}

	return converted, nil
}

// ApproveIssue approves the approval flow of the issue.
func (s *IssueService) ApproveIssue(ctx context.Context, request *v1pb.ApproveIssueRequest) (*v1pb.Issue, error) {
	issue, err := s.getIssueMessage(ctx, request.Name)
	if err != nil {
		return nil, err
	}
	payload := issue.Payload
	if payload.Approval == nil {
		return nil, status.Errorf(codes.Internal, "issue payload approval is nil")
	}
	if !payload.Approval.ApprovalFindingDone {
		return nil, status.Errorf(codes.FailedPrecondition, "approval template finding is not done")
	}
	if payload.Approval.ApprovalFindingError != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "approval template finding failed: %v", payload.Approval.ApprovalFindingError)
	}
	if len(payload.Approval.ApprovalTemplates) != 1 {
		return nil, status.Errorf(codes.Internal, "expecting one approval template but got %v", len(payload.Approval.ApprovalTemplates))
	}

	rejectedStep := utils.FindRejectedStep(payload.Approval.ApprovalTemplates[0], payload.Approval.Approvers)
	if rejectedStep != nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot approve because the issue has been rejected")
	}

	step := utils.FindNextPendingStep(payload.Approval.ApprovalTemplates[0], payload.Approval.Approvers)
	if step == nil {
		return nil, status.Errorf(codes.InvalidArgument, "the issue has been approved")
	}
	if len(step.Nodes) == 1 {
		node := step.Nodes[0]
		_, ok := node.Payload.(*storepb.ApprovalNode_ExternalNodeId)
		if ok {
			return s.updateExternalApprovalWithStatus(ctx, issue, relayplugin.StatusApproved)
		}
	}

	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}
	user, err := s.store.GetUserByID(ctx, principalID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to find user by id %v", principalID)
	}

	policy, err := s.store.GetProjectPolicy(ctx, &store.GetProjectPolicyMessage{UID: &issue.Project.UID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get project policy, error: %v", err)
	}

	canApprove, err := isUserReviewer(step, user, policy)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if principal can approve step, error: %v", err)
	}
	if !canApprove {
		return nil, status.Errorf(codes.PermissionDenied, "cannot approve because the user does not have the required permission")
	}

	payload.Approval.Approvers = append(payload.Approval.Approvers, &storepb.IssuePayloadApproval_Approver{
		Status:      storepb.IssuePayloadApproval_Approver_APPROVED,
		PrincipalId: int32(principalID),
	})

	approved, err := utils.CheckApprovalApproved(payload.Approval)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if the approval is approved, error: %v", err)
	}

	newApprovers, activityCreates, issueComments, err := utils.HandleIncomingApprovalSteps(ctx, s.store, s.relayRunner.Client, issue, payload.Approval)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to handle incoming approval steps, error: %v", err)
	}
	payload.Approval.Approvers = append(payload.Approval.Approvers, newApprovers...)

	issue, err = s.store.UpdateIssueV2(ctx, issue.UID, &store.UpdateIssueMessage{
		PayloadUpsert: &storepb.IssuePayload{
			Approval: payload.Approval,
		},
	}, api.SystemBotID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update issue, error: %v", err)
	}

	// Grant the privilege if the issue is approved.
	if approved && issue.Type == api.IssueGrantRequest {
		if err := utils.UpdateProjectPolicyFromGrantIssue(ctx, s.store, s.activityManager, issue, payload.GrantRequest); err != nil {
			return nil, err
		}
		userID, err := strconv.Atoi(strings.TrimPrefix(payload.GrantRequest.User, "users/"))
		if err != nil {
			return nil, err
		}
		newUser, err := s.store.GetUserByID(ctx, userID)
		if err != nil {
			return nil, err
		}
		// Post project IAM policy update activity.
		if _, err := s.activityManager.CreateActivity(ctx, &store.ActivityMessage{
			CreatorUID:        api.SystemBotID,
			ResourceContainer: issue.Project.GetName(),
			ContainerUID:      issue.Project.UID,
			Type:              api.ActivityProjectMemberCreate,
			Level:             api.ActivityInfo,
			Comment:           fmt.Sprintf("Granted %s to %s (%s).", newUser.Name, newUser.Email, payload.GrantRequest.Role),
		}, &activity.Metadata{}); err != nil {
			slog.Warn("Failed to create project activity", log.BBError(err))
		}
	}

	if err := func() error {
		p := &storepb.IssueCommentPayload{
			Comment: request.Comment,
			Event: &storepb.IssueCommentPayload_Approval_{
				Approval: &storepb.IssueCommentPayload_Approval{
					Status: storepb.IssueCommentPayload_Approval_APPROVED,
				},
			},
		}
		if err := s.store.CreateIssueComment(ctx, &store.IssueCommentMessage{
			IssueUID: issue.UID,
			Payload:  p,
		}, user.ID); err != nil {
			return err
		}
		for _, ic := range issueComments {
			if err := s.store.CreateIssueComment(ctx, ic, api.SystemBotID); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		slog.Warn("failed to create issue comment", log.BBError(err))
	}

	// It's ok to fail to create activity.
	if err := func() error {
		activityPayload, err := protojson.Marshal(&storepb.ActivityIssueCommentCreatePayload{
			Event: &storepb.ActivityIssueCommentCreatePayload_ApprovalEvent_{
				ApprovalEvent: &storepb.ActivityIssueCommentCreatePayload_ApprovalEvent{
					Status: storepb.ActivityIssueCommentCreatePayload_ApprovalEvent_APPROVED,
				},
			},
			IssueName: issue.Title,
		})
		if err != nil {
			return err
		}
		create := &store.ActivityMessage{
			CreatorUID:        principalID,
			ResourceContainer: issue.Project.GetName(),
			ContainerUID:      issue.UID,
			Type:              api.ActivityIssueCommentCreate,
			Level:             api.ActivityInfo,
			Comment:           request.Comment,
			Payload:           string(activityPayload),
		}
		if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{}); err != nil {
			return err
		}

		for _, create := range activityCreates {
			if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{}); err != nil {
				return err
			}
		}

		return nil
	}(); err != nil {
		slog.Error("failed to create skipping steps activity after approving issue", log.BBError(err))
	}

	if err := func() error {
		if len(payload.Approval.ApprovalTemplates) != 1 {
			return nil
		}
		approvalStep := utils.FindNextPendingStep(payload.Approval.ApprovalTemplates[0], payload.Approval.Approvers)
		if approvalStep == nil {
			return nil
		}
		protoPayload, err := protojson.Marshal(&storepb.ActivityIssueApprovalNotifyPayload{
			ApprovalStep: approvalStep,
		})
		if err != nil {
			return err
		}
		activityPayload, err := json.Marshal(api.ActivityIssueApprovalNotifyPayload{
			ProtoPayload: string(protoPayload),
		})
		if err != nil {
			return err
		}

		create := &store.ActivityMessage{
			CreatorUID:        api.SystemBotID,
			ResourceContainer: issue.Project.GetName(),
			ContainerUID:      issue.UID,
			Type:              api.ActivityIssueApprovalNotify,
			Level:             api.ActivityInfo,
			Comment:           "",
			Payload:           string(activityPayload),
		}
		if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{Issue: issue}); err != nil {
			return err
		}

		return nil
	}(); err != nil {
		slog.Error("failed to create approval step pending activity after creating issue", log.BBError(err))
	}

	func() {
		if !approved {
			return
		}

		// notify issue approved
		if err := func() error {
			create := &store.ActivityMessage{
				CreatorUID:        api.SystemBotID,
				ResourceContainer: issue.Project.GetName(),
				ContainerUID:      issue.UID,
				Type:              api.ActivityNotifyIssueApproved,
				Level:             api.ActivityInfo,
				Comment:           "",
				Payload:           "",
			}
			if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{
				Issue: issue,
			}); err != nil {
				return errors.Wrapf(err, "failed to create activity")
			}
			return nil
		}(); err != nil {
			slog.Error("failed to create activity for notifying issue approved", log.BBError(err))
		}

		// notify pipeline rollout
		if err := func() error {
			if issue.PipelineUID == nil {
				return nil
			}
			stages, err := s.store.ListStageV2(ctx, *issue.PipelineUID)
			if err != nil {
				return errors.Wrapf(err, "failed to list stages")
			}
			if len(stages) == 0 {
				return nil
			}
			policy, err := s.store.GetRolloutPolicy(ctx, stages[0].EnvironmentID)
			if err != nil {
				return errors.Wrapf(err, "failed to get rollout policy")
			}
			payload, err := json.Marshal(api.ActivityNotifyPipelineRolloutPayload{
				RolloutPolicy: policy,
				StageName:     stages[0].Name,
			})
			if err != nil {
				return errors.Wrapf(err, "failed to marshal activity payload")
			}
			create := &store.ActivityMessage{
				CreatorUID:        api.SystemBotID,
				ResourceContainer: issue.Project.GetName(),
				ContainerUID:      *issue.PipelineUID,
				Type:              api.ActivityNotifyPipelineRollout,
				Level:             api.ActivityInfo,
				Comment:           "",
				Payload:           string(payload),
			}
			if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{Issue: issue}); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			slog.Error("failed to create rollout release notification activity", log.BBError(err))
		}
	}()

	if issue.Type == api.IssueGrantRequest {
		if err := func() error {
			payload := issue.Payload
			approved, err := utils.CheckApprovalApproved(payload.Approval)
			if err != nil {
				return errors.Wrap(err, "failed to check if the approval is approved")
			}
			if approved {
				if err := utils.ChangeIssueStatus(ctx, s.store, s.activityManager, issue, api.IssueDone, api.SystemBotID, ""); err != nil {
					return errors.Wrap(err, "failed to update issue status")
				}
			}
			return nil
		}(); err != nil {
			slog.Debug("failed to update issue status to done if grant request issue is approved", log.BBError(err))
		}
	}

	issueV1, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return issueV1, nil
}

// RejectIssue rejects a issue.
func (s *IssueService) RejectIssue(ctx context.Context, request *v1pb.RejectIssueRequest) (*v1pb.Issue, error) {
	issue, err := s.getIssueMessage(ctx, request.Name)
	if err != nil {
		return nil, err
	}
	payload := issue.Payload
	if payload.Approval == nil {
		return nil, status.Errorf(codes.Internal, "issue payload approval is nil")
	}
	if !payload.Approval.ApprovalFindingDone {
		return nil, status.Errorf(codes.FailedPrecondition, "approval template finding is not done")
	}
	if payload.Approval.ApprovalFindingError != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "approval template finding failed: %v", payload.Approval.ApprovalFindingError)
	}
	if len(payload.Approval.ApprovalTemplates) != 1 {
		return nil, status.Errorf(codes.Internal, "expecting one approval template but got %v", len(payload.Approval.ApprovalTemplates))
	}

	rejectedStep := utils.FindRejectedStep(payload.Approval.ApprovalTemplates[0], payload.Approval.Approvers)
	if rejectedStep != nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot reject because the issue has been rejected")
	}

	step := utils.FindNextPendingStep(payload.Approval.ApprovalTemplates[0], payload.Approval.Approvers)
	if step == nil {
		return nil, status.Errorf(codes.InvalidArgument, "the issue has been approved")
	}
	if len(step.Nodes) == 1 {
		node := step.Nodes[0]
		_, ok := node.Payload.(*storepb.ApprovalNode_ExternalNodeId)
		if ok {
			return s.updateExternalApprovalWithStatus(ctx, issue, relayplugin.StatusRejected)
		}
	}

	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}
	user, err := s.store.GetUserByID(ctx, principalID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to find user by id %v", principalID)
	}

	policy, err := s.store.GetProjectPolicy(ctx, &store.GetProjectPolicyMessage{UID: &issue.Project.UID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get project policy, error: %v", err)
	}

	canApprove, err := isUserReviewer(step, user, policy)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if principal can reject step, error: %v", err)
	}
	if !canApprove {
		return nil, status.Errorf(codes.PermissionDenied, "cannot reject because the user does not have the required permission")
	}
	payload.Approval.Approvers = append(payload.Approval.Approvers, &storepb.IssuePayloadApproval_Approver{
		Status:      storepb.IssuePayloadApproval_Approver_REJECTED,
		PrincipalId: int32(principalID),
	})

	issue, err = s.store.UpdateIssueV2(ctx, issue.UID, &store.UpdateIssueMessage{
		PayloadUpsert: &storepb.IssuePayload{
			Approval: payload.Approval,
		},
	}, api.SystemBotID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update issue, error: %v", err)
	}

	if err := func() error {
		p := &storepb.IssueCommentPayload{
			Comment: request.Comment,
			Event: &storepb.IssueCommentPayload_Approval_{
				Approval: &storepb.IssueCommentPayload_Approval{
					Status: storepb.IssueCommentPayload_Approval_REJECTED,
				},
			},
		}
		return s.store.CreateIssueComment(ctx, &store.IssueCommentMessage{
			IssueUID: issue.UID,
			Payload:  p,
		}, user.ID)
	}(); err != nil {
		slog.Warn("failed to create issue comment", log.BBError(err))
	}

	// It's ok to fail to create activity.
	if err := func() error {
		activityPayload, err := protojson.Marshal(&storepb.ActivityIssueCommentCreatePayload{
			Event: &storepb.ActivityIssueCommentCreatePayload_ApprovalEvent_{
				ApprovalEvent: &storepb.ActivityIssueCommentCreatePayload_ApprovalEvent{
					Status: storepb.ActivityIssueCommentCreatePayload_ApprovalEvent_REJECTED,
				},
			},
			IssueName: issue.Title,
		})
		if err != nil {
			return err
		}
		create := &store.ActivityMessage{
			CreatorUID:        principalID,
			ResourceContainer: issue.Project.GetName(),
			ContainerUID:      issue.UID,
			Type:              api.ActivityIssueCommentCreate,
			Level:             api.ActivityInfo,
			Comment:           request.Comment,
			Payload:           string(activityPayload),
		}
		if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{}); err != nil {
			return err
		}
		return nil
	}(); err != nil {
		slog.Error("failed to create activity after rejecting issue", log.BBError(err))
	}

	issueV1, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return issueV1, nil
}

// RequestIssue requests a issue.
func (s *IssueService) RequestIssue(ctx context.Context, request *v1pb.RequestIssueRequest) (*v1pb.Issue, error) {
	issue, err := s.getIssueMessage(ctx, request.Name)
	if err != nil {
		return nil, err
	}
	payload := issue.Payload
	if payload.Approval == nil {
		return nil, status.Errorf(codes.Internal, "issue payload approval is nil")
	}
	if !payload.Approval.ApprovalFindingDone {
		return nil, status.Errorf(codes.FailedPrecondition, "approval template finding is not done")
	}
	if payload.Approval.ApprovalFindingError != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "approval template finding failed: %v", payload.Approval.ApprovalFindingError)
	}
	if len(payload.Approval.ApprovalTemplates) != 1 {
		return nil, status.Errorf(codes.Internal, "expecting one approval template but got %v", len(payload.Approval.ApprovalTemplates))
	}

	rejectedStep := utils.FindRejectedStep(payload.Approval.ApprovalTemplates[0], payload.Approval.Approvers)
	if rejectedStep == nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot request issues because the issue is not rejected")
	}

	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}
	user, err := s.store.GetUserByID(ctx, principalID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to find user by id %v", principalID)
	}

	canRequest := canRequestIssue(issue.Creator, user)
	if !canRequest {
		return nil, status.Errorf(codes.PermissionDenied, "cannot request issues because you are not the issue creator")
	}

	var newApprovers []*storepb.IssuePayloadApproval_Approver
	for _, approver := range payload.Approval.Approvers {
		if approver.Status == storepb.IssuePayloadApproval_Approver_REJECTED {
			continue
		}
		newApprovers = append(newApprovers, approver)
	}
	payload.Approval.Approvers = newApprovers

	newApprovers, activityCreates, issueComments, err := utils.HandleIncomingApprovalSteps(ctx, s.store, s.relayRunner.Client, issue, payload.Approval)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to handle incoming approval steps, error: %v", err)
	}
	payload.Approval.Approvers = append(payload.Approval.Approvers, newApprovers...)

	issue, err = s.store.UpdateIssueV2(ctx, issue.UID, &store.UpdateIssueMessage{
		PayloadUpsert: &storepb.IssuePayload{
			Approval: payload.Approval,
		},
	}, api.SystemBotID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update issue, error: %v", err)
	}

	if err := func() error {
		p := &storepb.IssueCommentPayload{
			Comment: request.Comment,
			Event: &storepb.IssueCommentPayload_Approval_{
				Approval: &storepb.IssueCommentPayload_Approval{
					Status: storepb.IssueCommentPayload_Approval_PENDING,
				},
			},
		}
		if err := s.store.CreateIssueComment(ctx, &store.IssueCommentMessage{
			IssueUID: issue.UID,
			Payload:  p,
		}, user.ID); err != nil {
			return err
		}
		for _, ic := range issueComments {
			if err := s.store.CreateIssueComment(ctx, ic, api.SystemBotID); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		slog.Warn("failed to create issue comment", log.BBError(err))
	}

	// It's ok to fail to create activity.
	if err := func() error {
		activityPayload, err := protojson.Marshal(&storepb.ActivityIssueCommentCreatePayload{
			Event: &storepb.ActivityIssueCommentCreatePayload_ApprovalEvent_{
				ApprovalEvent: &storepb.ActivityIssueCommentCreatePayload_ApprovalEvent{
					Status: storepb.ActivityIssueCommentCreatePayload_ApprovalEvent_PENDING,
				},
			},
			IssueName: issue.Title,
		})
		if err != nil {
			return err
		}
		create := &store.ActivityMessage{
			CreatorUID:        principalID,
			ResourceContainer: issue.Project.GetName(),
			ContainerUID:      issue.UID,
			Type:              api.ActivityIssueCommentCreate,
			Level:             api.ActivityInfo,
			Comment:           request.Comment,
			Payload:           string(activityPayload),
		}
		if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{}); err != nil {
			return err
		}

		for _, create := range activityCreates {
			if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{}); err != nil {
				return err
			}
		}

		return nil
	}(); err != nil {
		slog.Error("failed to create skipping steps activity after approving issue", log.BBError(err))
	}

	issueV1, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return issueV1, nil
}

// UpdateIssue updates the issue.
func (s *IssueService) UpdateIssue(ctx context.Context, request *v1pb.UpdateIssueRequest) (*v1pb.Issue, error) {
	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}
	if request.UpdateMask == nil {
		return nil, status.Errorf(codes.InvalidArgument, "update_mask must be set")
	}
	issue, err := s.getIssueMessage(ctx, request.Issue.Name)
	if err != nil {
		return nil, err
	}

	updateMasks := map[string]bool{}

	patch := &store.UpdateIssueMessage{}
	var activityCreates []*store.ActivityMessage
	var issueCommentCreates []*store.IssueCommentMessage
	for _, path := range request.UpdateMask.Paths {
		updateMasks[path] = true
		switch path {
		case "approval_finding_done":
			if request.Issue.ApprovalFindingDone {
				return nil, status.Errorf(codes.InvalidArgument, "cannot set approval_finding_done to true")
			}
			payload := issue.Payload
			if payload.Approval == nil {
				return nil, status.Errorf(codes.Internal, "issue payload approval is nil")
			}
			if !payload.Approval.ApprovalFindingDone {
				return nil, status.Errorf(codes.FailedPrecondition, "approval template finding is not done")
			}

			if patch.PayloadUpsert == nil {
				patch.PayloadUpsert = &storepb.IssuePayload{}
			}
			patch.PayloadUpsert.Approval = &storepb.IssuePayloadApproval{
				ApprovalFindingDone: false,
			}

			if issue.PlanUID != nil {
				plan, err := s.store.GetPlan(ctx, &store.FindPlanMessage{UID: issue.PlanUID})
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get plan, error: %v", err)
				}
				if plan == nil {
					return nil, status.Errorf(codes.NotFound, "plan %q not found", *issue.PlanUID)
				}

				planCheckRuns, err := getPlanCheckRunsFromPlan(ctx, s.store, plan)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get plan check runs for plan, error: %v", err)
				}
				if err := s.store.CreatePlanCheckRuns(ctx, planCheckRuns...); err != nil {
					return nil, status.Errorf(codes.Internal, "failed to create plan check runs, error: %v", err)
				}
			}

		case "title":
			patch.Title = &request.Issue.Title

			issueCommentCreates = append(issueCommentCreates, &store.IssueCommentMessage{
				IssueUID: issue.UID,
				Payload: &storepb.IssueCommentPayload{
					Event: &storepb.IssueCommentPayload_IssueUpdate_{
						IssueUpdate: &storepb.IssueCommentPayload_IssueUpdate{
							FromTitle: &issue.Title,
							ToTitle:   &request.Issue.Title,
						},
					},
				},
			})

			payload := &api.ActivityIssueFieldUpdatePayload{
				FieldID:   api.IssueFieldName,
				OldValue:  issue.Title,
				NewValue:  request.Issue.Title,
				IssueName: issue.Title,
			}
			activityPayload, err := json.Marshal(payload)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to marshal activity payload, error: %v", err)
			}
			activityCreates = append(activityCreates, &store.ActivityMessage{
				CreatorUID:        user.ID,
				ResourceContainer: issue.Project.GetName(),
				ContainerUID:      issue.UID,
				Type:              api.ActivityIssueFieldUpdate,
				Level:             api.ActivityInfo,
				Payload:           string(activityPayload),
			})

		case "description":
			patch.Description = &request.Issue.Description

			issueCommentCreates = append(issueCommentCreates, &store.IssueCommentMessage{
				IssueUID: issue.UID,
				Payload: &storepb.IssueCommentPayload{
					Event: &storepb.IssueCommentPayload_IssueUpdate_{
						IssueUpdate: &storepb.IssueCommentPayload_IssueUpdate{
							FromDescription: &issue.Description,
							ToDescription:   &request.Issue.Description,
						},
					},
				},
			})

			payload := &api.ActivityIssueFieldUpdatePayload{
				FieldID:   api.IssueFieldDescription,
				OldValue:  issue.Description,
				NewValue:  request.Issue.Description,
				IssueName: issue.Title,
			}
			activityPayload, err := json.Marshal(payload)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to marshal activity payload, error: %v", err)
			}
			activityCreates = append(activityCreates, &store.ActivityMessage{
				CreatorUID:        user.ID,
				ResourceContainer: issue.Project.GetName(),
				ContainerUID:      issue.UID,
				Type:              api.ActivityIssueFieldUpdate,
				Level:             api.ActivityInfo,
				Payload:           string(activityPayload),
			})

		case "subscribers":
			var subscribers []*store.UserMessage
			for _, subscriber := range request.Issue.Subscribers {
				subscriberEmail, err := common.GetUserEmail(subscriber)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "failed to get user email from %v, error: %v", subscriber, err)
				}
				user, err := s.store.GetUser(ctx, &store.FindUserMessage{Email: &subscriberEmail})
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get user %v, error: %v", subscriberEmail, err)
				}
				if user == nil {
					return nil, status.Errorf(codes.NotFound, "user %v not found", subscriber)
				}
				subscribers = append(subscribers, user)
			}
			patch.Subscribers = &subscribers

		case "assignee":
			var oldAssigneeID, oldAssigneeName string
			if issue.Assignee != nil {
				oldAssigneeID = strconv.Itoa(issue.Assignee.ID)
				oldAssigneeName = common.FormatUserEmail(issue.Assignee.Email)
			}
			if request.Issue.Assignee == "" {
				patch.UpdateAssignee = true
				patch.Assignee = nil
				payload := &api.ActivityIssueFieldUpdatePayload{
					FieldID:   api.IssueFieldAssignee,
					OldValue:  oldAssigneeID,
					NewValue:  "",
					IssueName: issue.Title,
				}
				activityPayload, err := json.Marshal(payload)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to marshal activity payload, error: %v", err)
				}
				activityCreates = append(activityCreates, &store.ActivityMessage{
					CreatorUID:        user.ID,
					ResourceContainer: issue.Project.GetName(),
					ContainerUID:      issue.UID,
					Type:              api.ActivityIssueFieldUpdate,
					Level:             api.ActivityInfo,
					Payload:           string(activityPayload),
				})
			} else {
				assigneeEmail, err := common.GetUserEmail(request.Issue.Assignee)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "failed to get user email from %v, error: %v", request.Issue.Assignee, err)
				}
				assignee, err := s.store.GetUser(ctx, &store.FindUserMessage{Email: &assigneeEmail})
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get user %v, error: %v", assigneeEmail, err)
				}
				if assignee == nil {
					return nil, status.Errorf(codes.NotFound, "user %v not found", request.Issue.Assignee)
				}
				patch.UpdateAssignee = true
				patch.Assignee = assignee

				payload := &api.ActivityIssueFieldUpdatePayload{
					FieldID:   api.IssueFieldAssignee,
					OldValue:  oldAssigneeID,
					NewValue:  strconv.Itoa(assignee.ID),
					IssueName: issue.Title,
				}
				activityPayload, err := json.Marshal(payload)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to marshal activity payload, error: %v", err)
				}
				activityCreates = append(activityCreates, &store.ActivityMessage{
					CreatorUID:        user.ID,
					ResourceContainer: issue.Project.GetName(),
					ContainerUID:      issue.UID,
					Type:              api.ActivityIssueFieldUpdate,
					Level:             api.ActivityInfo,
					Payload:           string(activityPayload),
				})
			}

			issueCommentCreates = append(issueCommentCreates, &store.IssueCommentMessage{
				IssueUID: issue.UID,
				Payload: &storepb.IssueCommentPayload{
					Event: &storepb.IssueCommentPayload_IssueUpdate_{
						IssueUpdate: &storepb.IssueCommentPayload_IssueUpdate{
							FromAssignee: &oldAssigneeName,
							ToAssignee:   &request.Issue.Assignee,
						},
					},
				},
			})
		case "labels":
			if len(request.Issue.Labels) == 0 {
				patch.RemoveLabels = true
			} else {
				if patch.PayloadUpsert == nil {
					patch.PayloadUpsert = &storepb.IssuePayload{}
				}
				patch.PayloadUpsert.Labels = request.Issue.Labels
			}
		}
	}

	ok, err = func() (bool, error) {
		if issue.Creator.ID == user.ID {
			return true, nil
		}
		ok, err := s.iamManager.CheckPermission(ctx, iam.PermissionIssuesUpdate, user, issue.Project.ResourceID)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}

		allowedUpdateMask, err := fieldmaskpb.New(request.Issue, "subscribers")
		if err != nil {
			return false, errors.Wrapf(err, "failed to new updateMask")
		}
		if issue.Assignee.ID == user.ID {
			if err := allowedUpdateMask.Append(request.Issue, "assignee"); err != nil {
				return false, errors.Wrapf(err, "failed to append update mask")
			}
		}

		allowedUpdateMask.Normalize()
		// request.UpdateMask is in allowedUpdateMask.
		if len(fieldmaskpb.Union(request.UpdateMask, allowedUpdateMask).GetPaths()) <= len(allowedUpdateMask.GetPaths()) {
			return true, nil
		}
		return false, nil
	}()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check permission, error: %v", err)
	}
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "permission denied, user does not have permission %q", iam.PermissionIssuesUpdate)
	}

	issue, err = s.store.UpdateIssueV2(ctx, issue.UID, patch, user.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update issue, error: %v", err)
	}

	if updateMasks["approval_finding_done"] {
		s.stateCfg.ApprovalFinding.Store(issue.UID, issue)
	}

	for _, create := range activityCreates {
		if _, err := s.activityManager.CreateActivity(ctx, create, &activity.Metadata{Issue: issue}); err != nil {
			slog.Warn("failed to create issue field update activity", "issue_id", issue.UID, log.BBError(err))
		}
	}
	for _, create := range issueCommentCreates {
		if err := s.store.CreateIssueComment(ctx, create, user.ID); err != nil {
			slog.Warn("failed to create issue comment", "issue id", issue.UID, log.BBError(err))
		}
	}

	issueV1, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return issueV1, nil
}

// BatchUpdateIssuesStatus batch updates issues status.
func (s *IssueService) BatchUpdateIssuesStatus(ctx context.Context, request *v1pb.BatchUpdateIssuesStatusRequest) (*v1pb.BatchUpdateIssuesStatusResponse, error) {
	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}

	var issueIDs []int
	var issues []*store.IssueMessage
	for _, issueName := range request.Issues {
		issue, err := s.getIssueMessage(ctx, issueName)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to find issue %v, err: %v", issueName, err)
		}
		if issue == nil {
			return nil, status.Errorf(codes.NotFound, "cannot find issue %v", issueName)
		}
		issueIDs = append(issueIDs, issue.UID)
		issues = append(issues, issue)

		ok, err := func() (bool, error) {
			if issue.Creator.ID == user.ID {
				return true, nil
			}
			return s.iamManager.CheckPermission(ctx, iam.PermissionIssuesUpdate, user, issue.Project.ResourceID)
		}()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to check if the user can update issue status, error: %v", err)
		}
		if !ok {
			return nil, status.Errorf(codes.PermissionDenied, "permission denied, user does not have permission %q for issue %q", iam.PermissionIssuesUpdate, issueName)
		}
	}

	if len(issueIDs) == 0 {
		return &v1pb.BatchUpdateIssuesStatusResponse{}, nil
	}

	newStatus, err := convertToAPIIssueStatus(request.Status)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to convert to issue status, err: %v", err)
	}

	if err := s.store.BatchUpdateIssueStatuses(ctx, issueIDs, newStatus, user.ID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to batch update issues, err: %v", err)
	}

	if err := func() error {
		var errs error
		for _, issue := range issues {
			updatedIssue, err := s.store.GetIssueV2(ctx, &store.FindIssueMessage{UID: &issue.UID})
			if err != nil {
				errs = multierr.Append(errs, errors.Wrapf(err, "failed to get issue %v", issue.UID))
				continue
			}

			func() {
				payload, err := json.Marshal(api.ActivityIssueStatusUpdatePayload{
					OldStatus: issue.Status,
					NewStatus: updatedIssue.Status,
					IssueName: updatedIssue.Title,
				})
				if err != nil {
					errs = multierr.Append(errs, errors.Wrapf(err, "failed to marshal activity after changing the issue status: %v", updatedIssue.Title))
					return
				}
				activityCreate := &store.ActivityMessage{
					CreatorUID:        user.ID,
					ResourceContainer: updatedIssue.Project.GetName(),
					ContainerUID:      updatedIssue.UID,
					Type:              api.ActivityIssueStatusUpdate,
					Level:             api.ActivityInfo,
					Comment:           request.Reason,
					Payload:           string(payload),
				}
				if _, err := s.activityManager.CreateActivity(ctx, activityCreate, &activity.Metadata{
					Issue: updatedIssue,
				}); err != nil {
					errs = multierr.Append(errs, errors.Wrapf(err, "failed to create activity after changing the issue status: %v", updatedIssue.Title))
					return
				}
			}()

			func() {
				fromStatus := convertToIssueStatus(issue.Status)
				if err := s.store.CreateIssueComment(ctx, &store.IssueCommentMessage{
					IssueUID: issue.UID,
					Payload: &storepb.IssueCommentPayload{
						Event: &storepb.IssueCommentPayload_IssueUpdate_{
							IssueUpdate: &storepb.IssueCommentPayload_IssueUpdate{
								FromStatus: convertToIssueCommentPayloadIssueUpdateIssueStatus(&fromStatus),
								ToStatus:   convertToIssueCommentPayloadIssueUpdateIssueStatus(&request.Status),
							},
						},
					},
				}, user.ID); err != nil {
					errs = multierr.Append(errs, errors.Wrapf(err, "failed to create issue comment after change the issue status"))
					return
				}
			}()
		}
		return errs
	}(); err != nil {
		slog.Error("failed to create activity after changing the issue status", log.BBError(err))
	}

	return &v1pb.BatchUpdateIssuesStatusResponse{}, nil
}

func (s *IssueService) ListIssueComments(ctx context.Context, request *v1pb.ListIssueCommentsRequest) (*v1pb.ListIssueCommentsResponse, error) {
	if request.PageSize < 0 {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("page size must be non-negative: %d", request.PageSize))
	}
	_, issueUID, err := common.GetProjectIDIssueUID(request.Parent)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	issue, err := s.store.GetIssueV2(ctx, &store.FindIssueMessage{UID: &issueUID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get issue, err: %v", err)
	}
	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}
	if err := s.canUserGetIssue(ctx, issue, user); err != nil {
		return nil, err
	}

	limit, offset, err := parseLimitAndOffset(request.PageToken, int(request.PageSize))
	if err != nil {
		return nil, err
	}
	limitPlusOne := limit + 1

	issueComments, err := s.store.ListIssueComment(ctx, &store.FindIssueCommentMessage{
		IssueUID: &issue.UID,
		Limit:    &limitPlusOne,
		Offset:   &offset,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list issue comments, err: %v", err)
	}
	var nextPageToken string
	if len(issueComments) == limitPlusOne {
		pageToken, err := getPageToken(limit, offset+limit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get next page token, error: %v", err)
		}
		nextPageToken = pageToken
		issueComments = issueComments[:limit]
	}

	return &v1pb.ListIssueCommentsResponse{
		IssueComments: convertToIssueComments(request.Parent, issueComments),
		NextPageToken: nextPageToken,
	}, nil
}

// CreateIssueComment creates the issue comment.
func (s *IssueService) CreateIssueComment(ctx context.Context, request *v1pb.CreateIssueCommentRequest) (*v1pb.IssueComment, error) {
	if request.IssueComment.Comment == "" {
		return nil, status.Errorf(codes.InvalidArgument, "issue comment is empty")
	}
	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}

	issue, err := s.getIssueMessage(ctx, request.Parent)
	if err != nil {
		return nil, err
	}

	ok, err = func() (bool, error) {
		if issue.Creator.ID == user.ID {
			return true, nil
		}
		return s.iamManager.CheckPermission(ctx, iam.PermissionIssueCommentsCreate, user, issue.Project.ResourceID)
	}()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check permission, error: %v", err)
	}
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "permission denied to create issue comment")
	}

	// TODO: migrate to store v2.
	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}
	activityCreate := &store.ActivityMessage{
		CreatorUID:        principalID,
		ResourceContainer: issue.Project.GetName(),
		ContainerUID:      issue.UID,
		Type:              api.ActivityIssueCommentCreate,
		Level:             api.ActivityInfo,
		Comment:           request.IssueComment.Comment,
	}

	var payload api.ActivityIssueCommentCreatePayload
	if activityCreate.Payload != "" {
		if err := json.Unmarshal([]byte(activityCreate.Payload), &payload); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to unmarshal payload: %v", err.Error())
		}
	}
	payload.IssueName = issue.Title
	bytes, err := json.Marshal(payload)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal payload: %v", err.Error())
	}
	activityCreate.Payload = string(bytes)

	activity, err := s.activityManager.CreateActivity(ctx, activityCreate, &activity.Metadata{Issue: issue})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create issue comment: %v", err.Error())
	}

	// TODO(p0ny): dual-write issue_comment and activity for now. Remove activity in the future.
	if err := s.store.CreateIssueComment(ctx, &store.IssueCommentMessage{
		IssueUID: issue.UID,
		Payload: &storepb.IssueCommentPayload{
			Comment: request.IssueComment.Comment,
		},
	}, principalID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create issue comment: %v", err)
	}

	return &v1pb.IssueComment{
		Uid:        fmt.Sprintf("%d", activity.UID),
		Comment:    activity.Comment,
		Payload:    activity.Payload,
		CreateTime: timestamppb.New(time.Unix(activity.CreatedTs, 0)),
		UpdateTime: timestamppb.New(time.Unix(activity.UpdatedTs, 0)),
	}, nil
}

// UpdateIssueComment updates the issue comment.
func (s *IssueService) UpdateIssueComment(ctx context.Context, request *v1pb.UpdateIssueCommentRequest) (*v1pb.IssueComment, error) {
	if request.UpdateMask.Paths == nil {
		return nil, status.Errorf(codes.InvalidArgument, "update_mask is required")
	}

	issue, err := s.getIssueMessage(ctx, request.Parent)
	if err != nil {
		return nil, err
	}
	issueCommentUID, err := strconv.Atoi(request.IssueComment.Uid)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid comment id %q: %v", request.IssueComment.Uid, err)
	}
	issueComment, err := s.store.GetIssueComment(ctx, &store.FindIssueCommentMessage{UID: &issueCommentUID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get issue comment: %v", err)
	}
	if issueComment == nil {
		return nil, status.Errorf(codes.NotFound, "issue comment not found")
	}

	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}

	ok, err = func() (bool, error) {
		if issueComment.Creator.ID == user.ID {
			return true, nil
		}
		return s.iamManager.CheckPermission(ctx, iam.PermissionIssueCommentsUpdate, user, issue.Project.ResourceID)
	}()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if the user has the permission, error: %v", err)
	}
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "permission denied, user does not have permission %q", iam.PermissionIssueCommentsUpdate)
	}

	update := &store.UpdateIssueCommentMessage{
		UID:       issueCommentUID,
		UpdaterID: user.ID,
	}
	for _, path := range request.UpdateMask.Paths {
		switch path {
		case "comment":
			update.Comment = &request.IssueComment.Comment
		default:
			return nil, status.Errorf(codes.InvalidArgument, `unsupport update_mask: "%s"`, path)
		}
	}

	if err := s.store.UpdateIssueComment(ctx, update); err != nil {
		if common.ErrorCode(err) == common.NotFound {
			return nil, status.Errorf(codes.NotFound, "cannot found the issue comment %s", request.IssueComment.Uid)
		}
		return nil, status.Errorf(codes.Internal, "failed to update the issue comment with error: %v", err.Error())
	}
	issueComment, err = s.store.GetIssueComment(ctx, &store.FindIssueCommentMessage{UID: &issueCommentUID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get issue comment: %v", err)
	}

	return convertToIssueComment(request.Parent, issueComment), nil
}

func (s *IssueService) getIssueMessage(ctx context.Context, name string) (*store.IssueMessage, error) {
	issueID, err := common.GetIssueID(name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	issue, err := s.store.GetIssueV2(ctx, &store.FindIssueMessage{UID: &issueID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get issue, error: %v", err)
	}
	if issue == nil {
		return nil, status.Errorf(codes.NotFound, "issue %d not found", issueID)
	}
	return issue, nil
}

func (s *IssueService) updateExternalApprovalWithStatus(ctx context.Context, issue *store.IssueMessage, approvalStatus relayplugin.Status) (*v1pb.Issue, error) {
	approval, err := s.store.GetExternalApprovalByIssueIDV2(ctx, issue.UID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get external approval for issue %v, error: %v", issue.UID, err)
	}
	if approvalStatus == relayplugin.StatusApproved {
		if err := s.relayRunner.ApproveExternalApprovalNode(ctx, issue.UID); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to approve external node, error: %v", err)
		}
	} else {
		if err := s.relayRunner.RejectExternalApprovalNode(ctx, issue.UID); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to reject external node, error: %v", err)
		}
	}

	if _, err := s.store.UpdateExternalApprovalV2(ctx, &store.UpdateExternalApprovalMessage{
		ID:        approval.ID,
		RowStatus: api.Archived,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update external approval, error: %v", err)
	}

	issueV1, err := convertToIssue(ctx, s.store, issue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert to issue, error: %v", err)
	}
	return issueV1, nil
}

func canRequestIssue(issueCreator *store.UserMessage, user *store.UserMessage) bool {
	return issueCreator.ID == user.ID
}

func isUserReviewer(step *storepb.ApprovalStep, user *store.UserMessage, policy *store.IAMPolicyMessage) (bool, error) {
	if len(step.Nodes) != 1 {
		return false, errors.Errorf("expecting one node but got %v", len(step.Nodes))
	}
	if step.Type != storepb.ApprovalStep_ANY {
		return false, errors.Errorf("expecting ANY step type but got %v", step.Type)
	}
	node := step.Nodes[0]
	if node.Type != storepb.ApprovalNode_ANY_IN_GROUP {
		return false, errors.Errorf("expecting ANY_IN_GROUP node type but got %v", node.Type)
	}

	roles, err := utils.GetUserFormattedRolesMap(user, policy)
	if err != nil {
		return false, errors.Wrapf(err, "failed to get user roles")
	}

	switch val := node.Payload.(type) {
	case *storepb.ApprovalNode_GroupValue_:
		switch val.GroupValue {
		case storepb.ApprovalNode_GROUP_VALUE_UNSPECIFILED:
			return false, errors.Errorf("invalid group value")
		case storepb.ApprovalNode_WORKSPACE_OWNER:
			return roles[common.FormatRole(api.WorkspaceAdmin.String())], nil
		case storepb.ApprovalNode_WORKSPACE_DBA:
			return roles[common.FormatRole(api.WorkspaceDBA.String())], nil
		case storepb.ApprovalNode_PROJECT_OWNER:
			return roles[common.FormatRole(api.ProjectOwner.String())], nil
		case storepb.ApprovalNode_PROJECT_MEMBER:
			return roles[common.FormatRole(api.ProjectDeveloper.String())], nil
		default:
			return false, errors.Errorf("invalid group value")
		}
	case *storepb.ApprovalNode_Role:
		return roles[val.Role], nil
	case *storepb.ApprovalNode_ExternalNodeId:
		return true, nil
	default:
		return false, errors.Errorf("invalid node payload type")
	}
}

// 1. if the user is the issue creator
// 2. with bb.issues.get/list permission, users can see grant request type issues.
// 3. with bb.issues.get/list and bb.plans.get/list permissions, users can see change database type issues.
func getIssuePermissionFilter(ctx context.Context, s *store.Store, user *store.UserMessage, iamManager *iam.Manager, p iam.Permission) (*store.FindIssueMessagePermissionFilter, error) {
	var planP iam.Permission
	switch p {
	case iam.PermissionIssuesList:
		planP = iam.PermissionPlansList
	case iam.PermissionIssuesGet:
		planP = iam.PermissionPlansGet
	default:
		return nil, errors.Errorf("unexpected permission %q", p)
	}

	projects, err := s.ListProjectV2(ctx, &store.FindProjectMessage{})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list projects")
	}
	var allProjectIDs []string
	for _, project := range projects {
		allProjectIDs = append(allProjectIDs, project.ResourceID)
	}

	issueProjectIDs, err := getProjectIDsWithPermission(ctx, s, user, iamManager, p)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get project ids with permission %q", p)
	}
	planProjectIDs, err := getProjectIDsWithPermission(ctx, s, user, iamManager, planP)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get project ids with permission %q", p)
	}

	// no filter, the user can see all.
	if issueProjectIDs == nil && planProjectIDs == nil {
		return nil, nil
	}

	intersectProjectIDs := func(array1 *[]string, array2 *[]string) *[]string {
		if array1 == nil && array2 == nil {
			return nil
		}
		if array1 == nil {
			return array2
		}
		if array2 == nil {
			return array1
		}
		res := intersect(*array1, *array2)
		return &res
	}

	allowGrantRequest := issueProjectIDs
	allowChangeDatabase := intersectProjectIDs(issueProjectIDs, planProjectIDs)
	if allowGrantRequest == nil {
		allowGrantRequest = &allProjectIDs
	}
	if allowChangeDatabase == nil {
		allowChangeDatabase = &allProjectIDs
	}

	res := &store.FindIssueMessagePermissionFilter{
		CreatorUID: user.ID,
	}
	for _, id := range *allowGrantRequest {
		res.ProjectIDs = append(res.ProjectIDs, id)
		res.IssueTypes = append(res.IssueTypes, api.IssueGrantRequest.String())
	}
	for _, id := range *allowChangeDatabase {
		res.ProjectIDs = append(res.ProjectIDs, id)
		res.IssueTypes = append(res.IssueTypes, api.IssueDatabaseGeneral.String())
	}
	return res, nil
}

func getProjectIDsWithPermission(ctx context.Context, s *store.Store, user *store.UserMessage, iamManager *iam.Manager, p iam.Permission) (*[]string, error) {
	ok, err := iamManager.CheckPermission(ctx, p, user)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to check permission %q", p)
	}
	if ok {
		return nil, nil
	}
	projects, err := s.ListProjectV2(ctx, &store.FindProjectMessage{})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list projects")
	}

	projectIDs := []string{}
	for _, project := range projects {
		ok, err := iamManager.CheckPermission(ctx, p, user, project.ResourceID)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to check permission %q", p)
		}
		if ok {
			projectIDs = append(projectIDs, project.ResourceID)
		}
	}
	return &projectIDs, nil
}

func intersect[T comparable](array1 []T, array2 []T) []T {
	res := []T{}
	seen := map[T]struct{}{}

	for _, e := range array1 {
		seen[e] = struct{}{}
	}

	for _, elem := range array2 {
		if _, ok := seen[elem]; ok {
			res = append(res, elem)
		}
	}

	return res
}
