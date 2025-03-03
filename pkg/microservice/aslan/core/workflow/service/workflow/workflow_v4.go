/*
Copyright 2022 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm/utils"
	"helm.sh/helm/v3/pkg/releaseutil"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/utils/pointer"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models/msg_queue"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models/template"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	templaterepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb/template"
	commonservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/collaboration"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/kube"
	larkservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/lark"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/repository"
	commomtemplate "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/template"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/webhook"
	commontypes "github.com/koderover/zadig/pkg/microservice/aslan/core/common/types"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/workflow/service/workflow/job"
	jobctl "github.com/koderover/zadig/pkg/microservice/aslan/core/workflow/service/workflow/job"
	"github.com/koderover/zadig/pkg/microservice/picket/client/opa"
	"github.com/koderover/zadig/pkg/setting"
	"github.com/koderover/zadig/pkg/shared/client/user"
	internalhandler "github.com/koderover/zadig/pkg/shared/handler"
	kubeclient "github.com/koderover/zadig/pkg/shared/kube/client"
	e "github.com/koderover/zadig/pkg/tool/errors"
	helmtool "github.com/koderover/zadig/pkg/tool/helmclient"
	"github.com/koderover/zadig/pkg/tool/kube/getter"
	"github.com/koderover/zadig/pkg/tool/kube/serializer"
	"github.com/koderover/zadig/pkg/tool/lark"
	"github.com/koderover/zadig/pkg/tool/log"
	"github.com/koderover/zadig/pkg/types"
)

func CreateWorkflowV4(user string, workflow *commonmodels.WorkflowV4, logger *zap.SugaredLogger) error {
	existedWorkflow, err := commonrepo.NewWorkflowV4Coll().Find(workflow.Name)
	if err == nil {
		errStr := fmt.Sprintf("与项目 [%s] 中的工作流 [%s] 标识相同", existedWorkflow.Project, existedWorkflow.DisplayName)
		return e.ErrUpsertWorkflow.AddDesc(errStr)
	}
	existedWorkflows, _, _ := commonrepo.NewWorkflowV4Coll().List(&commonrepo.ListWorkflowV4Option{ProjectName: workflow.Project, DisplayName: workflow.DisplayName}, 0, 0)
	if len(existedWorkflows) > 0 {
		errStr := fmt.Sprintf("当前项目已存在工作流 [%s]", workflow.DisplayName)
		return e.ErrUpsertWorkflow.AddDesc(errStr)
	}
	if err := LintWorkflowV4(workflow, logger); err != nil {
		return err
	}
	// lark approval different node type need different approval definition
	// check whether lark approvals in workflow need to create lark approval definition
	if err := createLarkApprovalDefinition(workflow); err != nil {
		return errors.Wrap(err, "create lark approval definition")
	}

	workflow.CreatedBy = user
	workflow.UpdatedBy = user
	workflow.CreateTime = time.Now().Unix()
	workflow.UpdateTime = time.Now().Unix()

	if err := jobctl.InstantiateWorkflow(workflow); err != nil {
		logger.Errorf("instantiate workflow error: %s", err)
		return e.ErrUpsertWorkflow.AddErr(err)
	}

	if _, err := commonrepo.NewWorkflowV4Coll().Create(workflow); err != nil {
		logger.Errorf("Failed to create workflow v4, the error is: %s", err)
		return e.ErrUpsertWorkflow.AddErr(err)
	}
	return nil
}

func SetWorkflowTasksCustomFields(projectName, workflowName string, args *models.CustomField, logger *zap.SugaredLogger) error {
	err := commonrepo.NewWorkflowV4Coll().SetCustomFields(commonrepo.SetCustomFieldsOptions{
		ProjectName:  projectName,
		WorkflowName: workflowName,
	}, args)
	if err != nil {
		logger.Errorf("Failed to set project: %s workflowV4: %s custom fields: %v, the error is: %s", projectName, workflowName, args, err)
		return e.ErrUpsertWorkflow.AddErr(err)
	}
	return nil
}

func GetWorkflowTasksCustomFields(projectName, workflowName string, logger *zap.SugaredLogger) (*models.CustomField, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().FindByOptions(commonrepo.WorkFlowOptions{
		ProjectName:  projectName,
		WorkflowName: workflowName,
	})
	if err != nil {
		logger.Errorf("Failed to get project: %s workflowV4: %s infos, the error is: %s", projectName, workflowName, err)
		return nil, e.ErrUpsertWorkflow.AddErr(err)
	}

	fields := workflow.CustomField
	if fields == nil {
		fields = &models.CustomField{
			TaskID:                 1,
			Status:                 1,
			Duration:               1,
			Executor:               1,
			BuildServiceComponent:  make(map[string]int),
			BuildCodeMsg:           make(map[string]int),
			DeployServiceComponent: make(map[string]int),
			DeployEnv:              make(map[string]int),
			TestResult:             make(map[string]int),
		}
	}

	buildJobNames, err := commonrepo.NewWorkflowV4Coll().GetJobNameList(projectName, workflowName, string(config.JobZadigBuild))
	if err != nil {
		if err != mongo.ErrNoDocuments && err != mongo.ErrNilDocument {
			return nil, err
		}
	}
	if fields.BuildServiceComponent == nil {
		fields.BuildServiceComponent = make(map[string]int)
	}
	for key := range fields.BuildServiceComponent {
		if !utils.Contains(buildJobNames, key) {
			delete(fields.BuildServiceComponent, key)
		}
	}

	if fields.BuildCodeMsg == nil {
		fields.BuildCodeMsg = make(map[string]int)
	}
	for key := range fields.BuildCodeMsg {
		if !utils.Contains(buildJobNames, key) {
			delete(fields.BuildCodeMsg, key)
		}
	}
	for _, jobName := range buildJobNames {
		if _, ok := fields.BuildServiceComponent[jobName]; !ok {
			fields.BuildServiceComponent[jobName] = 0
		}
		if _, ok := fields.BuildCodeMsg[jobName]; !ok {
			fields.BuildCodeMsg[jobName] = 0
		}
	}

	deployJobNames, err := commonrepo.NewWorkflowV4Coll().GetJobNameList(projectName, workflowName, string(config.JobZadigDeploy))
	if err != nil {
		if err != mongo.ErrNoDocuments && err != mongo.ErrNilDocument {
			return nil, err
		}
	}

	if fields.DeployServiceComponent == nil {
		fields.DeployServiceComponent = make(map[string]int)
	}
	for key := range fields.DeployServiceComponent {
		if !utils.Contains(deployJobNames, key) {
			delete(fields.DeployServiceComponent, key)
		}
	}

	if fields.DeployEnv == nil {
		fields.DeployEnv = make(map[string]int)
	}
	for key := range fields.DeployEnv {
		if !utils.Contains(deployJobNames, key) {
			delete(fields.DeployEnv, key)
		}
	}

	for _, jobName := range deployJobNames {
		if _, ok := fields.DeployServiceComponent[jobName]; !ok {
			fields.DeployServiceComponent[jobName] = 0
		}
		if _, ok := fields.DeployEnv[jobName]; !ok {
			fields.DeployEnv[jobName] = 0
		}
	}

	testJobNames, err := commonrepo.NewWorkflowV4Coll().GetJobNameList(projectName, workflowName, string(config.JobZadigTesting))
	if err != nil {
		if err != mongo.ErrNoDocuments && err != mongo.ErrNilDocument {
			return nil, err
		}
	}
	if fields.TestResult == nil {
		fields.TestResult = make(map[string]int)
	}
	for key := range fields.TestResult {
		if !utils.Contains(testJobNames, key) {
			delete(fields.TestResult, key)
		}
	}
	for _, jobName := range testJobNames {
		if _, ok := fields.TestResult[jobName]; !ok {
			fields.TestResult[jobName] = 0
		}
	}
	return fields, nil
}

func UpdateWorkflowV4(name, user string, inputWorkflow *commonmodels.WorkflowV4, logger *zap.SugaredLogger) error {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(name)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", name, err)
		return e.ErrFindWorkflow.AddErr(err)
	}
	if workflow.DisplayName != inputWorkflow.DisplayName {
		existedWorkflows, _, _ := commonrepo.NewWorkflowV4Coll().List(&commonrepo.ListWorkflowV4Option{ProjectName: workflow.Project, DisplayName: inputWorkflow.DisplayName}, 0, 0)
		if len(existedWorkflows) > 0 {
			errStr := fmt.Sprintf("workflow v4 [%s] 展示名称在当前项目下重复!", inputWorkflow.DisplayName)
			return e.ErrUpsertWorkflow.AddDesc(errStr)
		}
	}
	if err := LintWorkflowV4(inputWorkflow, logger); err != nil {
		return err
	}

	inputWorkflow.UpdatedBy = user
	inputWorkflow.UpdateTime = time.Now().Unix()
	inputWorkflow.ID = workflow.ID
	inputWorkflow.HookCtls = workflow.HookCtls
	inputWorkflow.JiraHookCtls = workflow.JiraHookCtls
	inputWorkflow.GeneralHookCtls = workflow.GeneralHookCtls
	inputWorkflow.MeegoHookCtls = workflow.MeegoHookCtls
	inputWorkflow.CustomField = workflow.CustomField

	for _, stage := range inputWorkflow.Stages {
		for _, job := range stage.Jobs {
			if err := jobctl.Instantiate(job, workflow); err != nil {
				logger.Errorf("Failed to instantiate workflow v4,error: %v", err)
				return e.ErrUpsertWorkflow.AddErr(err)
			}
		}
	}
	// lark approval different node type need different approval definition
	// check whether lark approvals in workflow need to create lark approval definition
	if err := createLarkApprovalDefinition(inputWorkflow); err != nil {
		return errors.Wrap(err, "create lark approval definition")
	}

	if err := commonrepo.NewWorkflowV4Coll().Update(
		workflow.ID.Hex(),
		inputWorkflow,
	); err != nil {
		logger.Errorf("update workflowV4 error: %s", err)
		return e.ErrUpsertWorkflow.AddErr(err)
	}
	return nil
}

func FindWorkflowV4(encryptedKey, name string, logger *zap.SugaredLogger) (*commonmodels.WorkflowV4, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(name)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", name, err)
		return workflow, e.ErrFindWorkflow.AddErr(err)
	}
	if err := jobctl.InstantiateWorkflow(workflow); err != nil {
		logger.Errorf("instantiate workflow error: %s", err)
		return workflow, e.ErrFindWorkflow.AddErr(err)
	}

	if err := ensureWorkflowV4Resp(encryptedKey, workflow, logger); err != nil {
		return workflow, err
	}
	return workflow, err
}

func FindWorkflowV4Raw(name string, logger *zap.SugaredLogger) (*commonmodels.WorkflowV4, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(name)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", name, err)
		return workflow, e.ErrFindWorkflow.AddErr(err)
	}
	return workflow, err
}

func DeleteWorkflowV4(name string, logger *zap.SugaredLogger) error {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(name)
	if err != nil {
		logger.Errorf("Failed to delete WorkflowV4: %s, the error is: %v", name, err)
		return e.ErrDeleteWorkflow.AddErr(err)
	}
	if err := commonrepo.NewWorkflowV4Coll().DeleteByID(workflow.ID.Hex()); err != nil {
		logger.Errorf("Failed to delete WorkflowV4: %s, the error is: %v", name, err)
		return e.ErrDeleteWorkflow.AddErr(err)
	}
	if err := commonrepo.NewworkflowTaskv4Coll().DeleteByWorkflowName(name); err != nil {
		logger.Errorf("Failed to delete WorkflowV4 task: %s, the error is: %v", name, err)
		return e.ErrDeleteWorkflow.AddErr(err)
	}
	if err := commonrepo.NewCounterColl().Delete("WorkflowTaskV4:" + name); err != nil {
		log.Errorf("Counter.Delete error: %s", err)
	}
	return nil
}

func ListWorkflowV4(projectName, viewName, userID string, names, v4Names []string, policyFound bool, logger *zap.SugaredLogger) ([]*Workflow, error) {
	resp := make([]*Workflow, 0)
	var err error
	ignoreWorkflow := false
	ignoreWorkflowV4 := false
	if viewName == "" {
		if policyFound && len(names) == 0 {
			ignoreWorkflow = true
		}
		if policyFound && len(v4Names) == 0 {
			ignoreWorkflowV4 = true
		}
	} else {
		names, v4Names, err = filterWorkflowNamesByView(projectName, viewName, names, v4Names, policyFound)
		if err != nil {
			logger.Errorf("filterWorkflowNames error: %s", err)
			return resp, err
		}
		if len(names) == 0 {
			ignoreWorkflow = true
		}
		if len(v4Names) == 0 {
			ignoreWorkflowV4 = true
		}
	}
	workflowV4List := []*commonmodels.WorkflowV4{}
	if !ignoreWorkflowV4 {
		workflowV4List, _, err = commonrepo.NewWorkflowV4Coll().List(&commonrepo.ListWorkflowV4Option{
			ProjectName: projectName,
			Names:       v4Names,
		}, 0, 0)
		if err != nil {
			logger.Errorf("Failed to list workflow v4, the error is: %s", err)
			return resp, err
		}
	}

	workflow := []*Workflow{}

	// distribute center only surpport custom workflow.
	if !ignoreWorkflow && projectName != setting.EnterpriseProject {
		workflow, err = ListWorkflows([]string{projectName}, userID, names, logger)
		if err != nil {
			return resp, err
		}
	}

	workflowList := []string{}
	for _, wV4 := range workflowV4List {
		workflowList = append(workflowList, wV4.Name)
	}
	resp = append(resp, workflow...)
	workflowCMMap, err := collaboration.GetWorkflowCMMap([]string{projectName}, logger)
	if err != nil {
		return nil, err
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		tasks []*models.WorkflowTask
	)
	for _, name := range workflowList {
		wg.Add(1)
		go func(workflowName string) {
			defer wg.Done()
			resp, _, err2 := commonrepo.NewworkflowTaskv4Coll().List(&commonrepo.ListWorkflowTaskV4Option{
				WorkflowName: workflowName,
				Limit:        10,
			})
			if err2 != nil {
				err = err2
				return
			}
			mu.Lock()
			defer mu.Unlock()
			tasks = append(tasks, resp...)
		}(name)
	}
	wg.Wait()
	if err != nil {
		return resp, err
	}

	favorites, err := commonrepo.NewFavoriteColl().List(&commonrepo.FavoriteArgs{UserID: userID, Type: string(config.WorkflowTypeV4)})
	if err != nil {
		return resp, errors.Errorf("failed to get custom workflow favorite data, err: %v", err)
	}
	favoriteSet := sets.NewString()
	for _, f := range favorites {
		favoriteSet.Insert(f.Name)
	}
	workflowStatMap := getWorkflowStatMap(workflowList, config.WorkflowTypeV4)

	for _, workflowModel := range workflowV4List {
		stages := []string{}
		for _, stage := range workflowModel.Stages {
			if stage.Approval != nil && stage.Approval.Enabled {
				stages = append(stages, "人工审批")
			}
			stages = append(stages, stage.Name)
		}
		var baseRefs []string
		if cmSet, ok := workflowCMMap[collaboration.BuildWorkflowCMMapKey(workflowModel.Project, workflowModel.Name)]; ok {
			for _, cm := range cmSet.List() {
				baseRefs = append(baseRefs, cm)
			}
		}
		workflow := &Workflow{
			Name:          workflowModel.Name,
			DisplayName:   workflowModel.DisplayName,
			ProjectName:   workflowModel.Project,
			EnabledStages: stages,
			CreateTime:    workflowModel.CreateTime,
			UpdateTime:    workflowModel.UpdateTime,
			UpdateBy:      workflowModel.UpdatedBy,
			WorkflowType:  setting.CustomWorkflowType,
			Description:   workflowModel.Description,
			BaseRefs:      baseRefs,
			BaseName:      workflowModel.BaseName,
		}
		if workflowModel.Category == setting.ReleaseWorkflow {
			workflow.WorkflowType = string(setting.ReleaseWorkflow)
		}
		if favoriteSet.Has(workflow.Name) {
			workflow.IsFavorite = true
		}
		getRecentTaskV4Info(workflow, tasks)
		setWorkflowStat(workflow, workflowStatMap)

		resp = append(resp, workflow)
	}
	return resp, nil
}

type NameWithParams struct {
	Name        string                `json:"name"`
	DisplayName string                `json:"display_name"`
	ProjectName string                `json:"project_name"`
	Params      []*commonmodels.Param `json:"params"`
}

func ListWorkflowV4CanTrigger(ctx *internalhandler.Context) ([]*NameWithParams, error) {
	var workflowList []*models.WorkflowV4
	var err error

	if ctx.Resources.IsSystemAdmin {
		// if a user is system admin, we simply list all custom workflows
		workflowList, _, err = commonrepo.NewWorkflowV4Coll().List(&commonrepo.ListWorkflowV4Option{}, 0, 0)
		if err != nil {
			ctx.Logger.Errorf("Failed to list all custom workflows from system, err: %s", err)
			return nil, errors.Errorf("failed to list workflow v4, the error is: %s", err)
		}
	} else {
		projects, _, projectFindErr := user.New().ListAuthorizedProjects(ctx.UserID)
		if projectFindErr != nil {
			ctx.Logger.Errorf("failed to list authorized project for normal user, err: %s", projectFindErr)
			return nil, errors.New("failed to get allowed projects")
		}

		for _, authorizedProject := range projects {
			// since it is in authorized project list, it either has a role in this project, or it is in a collaboration mode.
			if projectAuthInfo, ok := ctx.Resources.ProjectAuthInfo[authorizedProject]; ok {
				// if it has a project role, check it
				if projectAuthInfo.IsProjectAdmin || projectAuthInfo.Workflow.View {
					projectedWorkflowList, _, workflowFindErr := commonrepo.NewWorkflowV4Coll().List(&commonrepo.ListWorkflowV4Option{
						ProjectName: authorizedProject,
					}, 0, 0)
					if workflowFindErr != nil {
						ctx.Logger.Errorf("failed to find authorized workflows for project: %s, error is: %s", authorizedProject, workflowFindErr)
						return nil, errors.New("failed to get allowed workflows")
					}
					workflowList = append(workflowList, projectedWorkflowList...)
				} else {
					// if it does not have any kind of authz from role, we check from collaboration mode anyway.
					authorizedProjectedCustomWorkflow, findErr := getAuthorizedCustomWorkflowFromCollaborationMode(ctx, authorizedProject)
					if findErr != nil {
						ctx.Logger.Errorf("failed to find authorized workflows for project: %s, error is: %s", authorizedProject, findErr)
						return nil, errors.New("failed to get allowed workflows")
					}
					workflowList = append(workflowList, authorizedProjectedCustomWorkflow...)
				}
			} else {
				// otherwise simply check for collaboration mode will suffice
				authorizedProjectedCustomWorkflow, findErr := getAuthorizedCustomWorkflowFromCollaborationMode(ctx, authorizedProject)
				if findErr != nil {
					ctx.Logger.Errorf("failed to find authorized workflows for project: %s, error is: %s", authorizedProject, findErr)
					return nil, errors.New("failed to get allowed workflows")
				}
				workflowList = append(workflowList, authorizedProjectedCustomWorkflow...)
			}

		}
	}

	var result []*NameWithParams
LOOP:
	for _, workflowV4 := range workflowList {
		for _, stage := range workflowV4.Stages {
			for _, job := range stage.Jobs {
				switch job.JobType {
				case config.JobFreestyle, config.JobPlugin, config.JobWorkflowTrigger:
				default:
					continue LOOP
				}
			}
		}
		var paramList []*commonmodels.Param
		for _, param := range workflowV4.Params {
			if !strings.Contains(param.Value, setting.FixedValueMark) {
				paramList = append(paramList, param)
			}
		}
		result = append(result, &NameWithParams{
			Name:        workflowV4.Name,
			DisplayName: workflowV4.DisplayName,
			ProjectName: workflowV4.Project,
			Params:      paramList,
		})
	}
	return result, nil
}

func getAuthorizedCustomWorkflowFromCollaborationMode(ctx *internalhandler.Context, projectKey string) ([]*models.WorkflowV4, error) {
	_, authorizedCustomWorkflow, workflowFindErr := user.New().ListAuthorizedWorkflows(ctx.UserID, projectKey)
	if workflowFindErr != nil {
		ctx.Logger.Errorf("failed to find authorized workflows for project: %s, error is: %s", projectKey, workflowFindErr)
		return nil, errors.New("failed to get allowed workflows")
	}
	if len(authorizedCustomWorkflow) > 0 {
		// if there are authorized workflow, we get the details out of it
		projectedWorkflowList, _, workflowFindErr := commonrepo.NewWorkflowV4Coll().List(&commonrepo.ListWorkflowV4Option{
			ProjectName: projectKey,
			Names:       authorizedCustomWorkflow,
		}, 0, 0)
		if workflowFindErr != nil {
			ctx.Logger.Errorf("failed to find authorized workflows for project: %s, error is: %s", projectKey, workflowFindErr)
			return nil, errors.New("failed to get allowed workflows")
		}
		return projectedWorkflowList, nil
	}
	return make([]*models.WorkflowV4, 0), nil
}

func filterWorkflowNamesByView(projectName, viewName string, workflowNames, workflowV4Names []string, policyFound bool) ([]string, []string, error) {
	if viewName == "" {
		return workflowNames, workflowV4Names, nil
	}
	view, err := commonrepo.NewWorkflowViewColl().Find(projectName, viewName)
	if err != nil {
		return workflowNames, workflowV4Names, err
	}
	enabledWorkflow := []string{}
	enabledWorkflowV4 := []string{}
	for _, workflow := range view.Workflows {
		if !workflow.Enabled {
			continue
		}
		if workflow.WorkflowType == setting.CustomWorkflowType {
			enabledWorkflowV4 = append(enabledWorkflowV4, workflow.WorkflowName)
		} else {
			enabledWorkflow = append(enabledWorkflow, workflow.WorkflowName)
		}
	}
	if !policyFound {
		return enabledWorkflow, enabledWorkflowV4, nil
	}
	return intersection(workflowNames, enabledWorkflow), intersection(workflowV4Names, enabledWorkflowV4), nil
}

func intersection(a, b []string) []string {
	m := make(map[string]bool)
	var intersection []string
	for _, item := range a {
		m[item] = true
	}
	for _, item := range b {
		if _, ok := m[item]; ok {
			intersection = append(intersection, item)
		}
	}
	return intersection
}

func getRecentTaskV4Info(workflow *Workflow, tasks []*commonmodels.WorkflowTask) {
	recentTask := &commonmodels.WorkflowTask{}
	recentFailedTask := &commonmodels.WorkflowTask{}
	recentSucceedTask := &commonmodels.WorkflowTask{}
	workflow.NeverRun = true
	var workflowList []*commonmodels.WorkflowTask
	var recentTenTask []*commonmodels.WorkflowTask
	for _, task := range tasks {
		if task.WorkflowName != workflow.Name {
			continue
		}
		workflowList = append(workflowList, task)
		workflow.NeverRun = false
	}
	sort.Slice(workflowList, func(i, j int) bool {
		return workflowList[i].TaskID > workflowList[j].TaskID
	})
	for _, task := range workflowList {
		if recentSucceedTask.TaskID != 0 && recentFailedTask.TaskID != 0 && len(recentTenTask) == 10 {
			break
		}
		if recentTask.TaskID == 0 {
			recentTask = task
		}
		if task.Status == config.StatusPassed && recentSucceedTask.TaskID == 0 {
			recentSucceedTask = task
		}
		if task.Status == config.StatusFailed && recentFailedTask.TaskID == 0 {
			recentFailedTask = task
		}
		if len(recentTenTask) < 10 {
			recentTenTask = append(recentTenTask, task)
		}
	}
	if recentTask.TaskID > 0 {
		workflow.RecentTask = &TaskInfo{
			TaskID:       recentTask.TaskID,
			PipelineName: recentTask.WorkflowName,
			Status:       string(recentTask.Status),
			TaskCreator:  recentTask.TaskCreator,
			CreateTime:   recentTask.CreateTime,
		}
	}
	if recentSucceedTask.TaskID > 0 {
		workflow.RecentSuccessfulTask = &TaskInfo{
			TaskID:       recentSucceedTask.TaskID,
			PipelineName: recentSucceedTask.WorkflowName,
			Status:       string(recentSucceedTask.Status),
			TaskCreator:  recentSucceedTask.TaskCreator,
			CreateTime:   recentSucceedTask.CreateTime,
		}
	}
	if recentFailedTask.TaskID > 0 {
		workflow.RecentFailedTask = &TaskInfo{
			TaskID:       recentFailedTask.TaskID,
			PipelineName: recentFailedTask.WorkflowName,
			Status:       string(recentFailedTask.Status),
			TaskCreator:  recentFailedTask.TaskCreator,
			CreateTime:   recentFailedTask.CreateTime,
		}
	}
	if len(recentTenTask) > 0 {
		for _, task := range recentTenTask {
			workflow.RecentTasks = append(workflow.RecentTasks, &TaskInfo{
				TaskID:      task.TaskID,
				Status:      string(task.Status),
				TaskCreator: task.TaskCreator,
				CreateTime:  task.CreateTime,
				StartTime:   task.StartTime,
				EndTime:     task.EndTime,
			})
		}
	}
}

func clearWorkflowV4Triggers(workflow *commonmodels.WorkflowV4) {
	workflow.HookCtls = nil
	workflow.MeegoHookCtls = nil
	workflow.GeneralHookCtls = nil
	workflow.JiraHookCtls = nil
}

func ensureWorkflowV4Resp(encryptedKey string, workflow *commonmodels.WorkflowV4, logger *zap.SugaredLogger) error {
	for _, stage := range workflow.Stages {
		for _, job := range stage.Jobs {
			if job.JobType == config.JobZadigBuild {
				spec := &commonmodels.ZadigBuildJobSpec{}
				if err := commonmodels.IToi(job.Spec, spec); err != nil {
					logger.Errorf(err.Error())
					return e.ErrFindWorkflow.AddErr(err)
				}
				for _, build := range spec.ServiceAndBuilds {
					buildInfo, err := commonrepo.NewBuildColl().Find(&commonrepo.BuildFindOption{Name: build.BuildName})
					if err != nil {
						logger.Errorf("find build: %s error: %s", build.BuildName, err)
						continue
					}
					kvs := buildInfo.PreBuild.Envs
					if buildInfo.TemplateID != "" {
						templateEnvs := []*commonmodels.KeyVal{}
						buildTemplate, err := commonrepo.NewBuildTemplateColl().Find(&commonrepo.BuildTemplateQueryOption{
							ID: buildInfo.TemplateID,
						})
						// if template not found, envs are empty, but do not block user.
						if err != nil {
							logger.Error("build job: %s, template not found", buildInfo.Name)
						} else {
							templateEnvs = buildTemplate.PreBuild.Envs
						}

						for _, target := range buildInfo.Targets {
							if target.ServiceName == build.ServiceName && target.ServiceModule == build.ServiceModule {
								kvs = target.Envs
							}
						}
						// if build template update any keyvals, merge it.
						kvs = commonservice.MergeBuildEnvs(templateEnvs, kvs)
					}
					build.KeyVals = commonservice.MergeBuildEnvs(kvs, build.KeyVals)
					if err := commonservice.EncryptKeyVals(encryptedKey, build.KeyVals, logger); err != nil {
						logger.Errorf(err.Error())
						return e.ErrFindWorkflow.AddErr(err)
					}
				}
				job.Spec = spec
			}
			if job.JobType == config.JobWorkflowTrigger {
				spec := &commonmodels.WorkflowTriggerJobSpec{}
				if err := commonmodels.IToi(job.Spec, spec); err != nil {
					logger.Errorf(err.Error())
					return e.ErrFindWorkflow.AddErr(err)
				}
				for _, info := range spec.ServiceTriggerWorkflow {
					workflow, err := commonrepo.NewWorkflowV4Coll().Find(info.WorkflowName)
					if err != nil {
						logger.Errorf(err.Error())
						continue
					}
					var paramList []*commonmodels.Param
					for _, param := range workflow.Params {
						if !strings.Contains(param.Value, setting.FixedValueMark) {
							param.Source = config.ParamSourceRuntime
							paramList = append(paramList, param)
						}
					}
					info.Params = commonservice.MergeParams(paramList, info.Params)
				}
				for _, info := range spec.FixedWorkflowList {
					workflow, err := commonrepo.NewWorkflowV4Coll().Find(info.WorkflowName)
					if err != nil {
						logger.Errorf(err.Error())
						continue
					}
					var paramList []*commonmodels.Param
					for _, param := range workflow.Params {
						if !strings.Contains(param.Value, setting.FixedValueMark) {
							param.Source = config.ParamSourceRuntime
							paramList = append(paramList, param)
						}
					}
					info.Params = commonservice.MergeParams(paramList, info.Params)
				}
				job.Spec = spec
			}
			if job.JobType == config.JobFreestyle {
				spec := &commonmodels.FreestyleJobSpec{}
				if err := commonmodels.IToi(job.Spec, spec); err != nil {
					logger.Errorf(err.Error())
					return e.ErrFindWorkflow.AddErr(err)
				}
				if err := commonservice.EncryptKeyVals(encryptedKey, spec.Properties.Envs, logger); err != nil {
					logger.Errorf(err.Error())
					return e.ErrFindWorkflow.AddErr(err)
				}
				job.Spec = spec
			}
			if job.JobType == config.JobPlugin {
				spec := &commonmodels.PluginJobSpec{}
				if err := commonmodels.IToi(job.Spec, spec); err != nil {
					logger.Errorf(err.Error())
					return e.ErrFindWorkflow.AddErr(err)
				}
				if err := commonservice.EncryptParams(encryptedKey, spec.Plugin.Inputs, logger); err != nil {
					logger.Errorf(err.Error())
					return e.ErrFindWorkflow.AddErr(err)
				}
				job.Spec = spec
			}
			if job.JobType == config.JobZadigTesting {
				spec := &commonmodels.ZadigTestingJobSpec{}
				if err := commonmodels.IToi(job.Spec, spec); err != nil {
					logger.Errorf(err.Error())
					return e.ErrFindWorkflow.AddErr(err)
				}
				job.Spec = spec
				for _, testing := range spec.ServiceAndTests {
					testingInfo, err := commonrepo.NewTestingColl().Find(testing.Name, "")
					if err != nil {
						logger.Errorf("find testing: %s error: %s", testing.Name, err)
						continue
					}
					testing.KeyVals = commonservice.MergeBuildEnvs(testingInfo.PreTest.Envs, testing.KeyVals)
				}
				for _, testing := range spec.TestModules {
					testingInfo, err := commonrepo.NewTestingColl().Find(testing.Name, "")
					if err != nil {
						logger.Errorf("find testing: %s error: %s", testing.Name, err)
						continue
					}
					testing.KeyVals = commonservice.MergeBuildEnvs(testingInfo.PreTest.Envs, testing.KeyVals)
				}
			}
		}
	}
	return nil
}

func LintWorkflowV4(workflow *commonmodels.WorkflowV4, logger *zap.SugaredLogger) error {
	if workflow.Project == "" {
		err := fmt.Errorf("project should not be empty")
		logger.Errorf(err.Error())
		return e.ErrUpsertWorkflow.AddErr(err)
	}
	match, err := regexp.MatchString(setting.WorkflowRegx, workflow.Name)
	if err != nil {
		logger.Errorf("reg compile failed: %v", err)
		return e.ErrUpsertWorkflow.AddErr(err)
	}
	if !match {
		errMsg := "工作流标识支持小写字母、数字和中划线"
		logger.Error(errMsg)
		return e.ErrUpsertWorkflow.AddDesc(errMsg)
	}

	project := &template.Product{}
	// for deploy center workflow, it doesn't belongs to any project, so we use a specical project name to distinguish it.
	if workflow.Project != setting.EnterpriseProject {
		project, err = templaterepo.NewProductColl().Find(workflow.Project)
		if err != nil {
			logger.Errorf("Failed to get project %s, error: %v", workflow.Project, err)
			return e.ErrUpsertWorkflow.AddErr(err)
		}
	}

	if project.ProductFeature != nil {
		if project.ProductFeature.DeployType != setting.K8SDeployType && project.ProductFeature.DeployType != setting.HelmDeployType {
			logger.Error("common workflow only support k8s and helm project")
			return e.ErrUpsertWorkflow.AddDesc("common workflow only support k8s and helm project")
		}
	}
	stageNameMap := make(map[string]bool)
	jobNameMap := make(map[string]string)

	reg, err := regexp.Compile(setting.JobNameRegx)
	if err != nil {
		logger.Errorf("reg compile failed: %v", err)
		return e.ErrUpsertWorkflow.AddErr(err)
	}
	for _, stage := range workflow.Stages {
		if err := lintApprovals(stage.Approval); err != nil {
			logger.Errorf("stage: %s approval info error: %v", stage.Name, err)
			return e.ErrUpsertWorkflow.AddDesc(fmt.Sprintf("stage: %s approval info error: %v", stage.Name, err))
		}
		if _, ok := stageNameMap[stage.Name]; !ok {
			stageNameMap[stage.Name] = true
		} else {
			logger.Errorf("duplicated stage name: %s", stage.Name)
			return e.ErrUpsertWorkflow.AddDesc(fmt.Sprintf("duplicated stage name: %s", stage.Name))
		}
		for _, job := range stage.Jobs {
			if match := reg.MatchString(job.Name); !match {
				logger.Errorf("job name [%s] did not match %s", job.Name, setting.JobNameRegx)
				return e.ErrUpsertWorkflow.AddDesc(fmt.Sprintf("job name [%s] did not match %s", job.Name, setting.JobNameRegx))
			}
			if _, ok := jobNameMap[job.Name]; !ok {
				jobNameMap[job.Name] = string(job.JobType)
			} else {
				logger.Errorf("duplicated job name: %s", job.Name)
				return e.ErrUpsertWorkflow.AddDesc(fmt.Sprintf("duplicated job name: %s", job.Name))
			}
			if err := jobctl.LintJob(job, workflow); err != nil {
				logger.Errorf("lint job %s failed: %v", job.Name, err)
				return e.ErrUpsertWorkflow.AddErr(err)
			}
		}
	}
	return nil
}

func lintApprovals(approval *commonmodels.Approval) error {
	if approval == nil {
		return nil
	}
	if !approval.Enabled {
		return nil
	}
	switch approval.Type {
	case config.NativeApproval:
		if approval.NativeApproval == nil {
			return errors.New("approval not found")
		}
		if len(approval.NativeApproval.ApproveUsers) < approval.NativeApproval.NeededApprovers {
			return errors.New("all approve users should not less than needed approvers")
		}
	case config.LarkApproval:
		if approval.LarkApproval == nil {
			return errors.New("approval not found")
		}
		if len(approval.LarkApproval.ApprovalNodes) == 0 {
			return errors.New("num of approval-node is 0")
		}
		for i, node := range approval.LarkApproval.ApprovalNodes {
			if len(node.ApproveUsers) == 0 {
				return errors.Errorf("num of approval-node %d approver is 0", i)
			}
			if !lo.Contains([]string{"AND", "OR"}, string(node.Type)) {
				return errors.Errorf("approval-node %d type should be AND or OR", i)
			}
		}
	case config.DingTalkApproval:
		if approval.DingTalkApproval == nil {
			return errors.New("approval not found")
		}
		userIDSets := sets.NewString()
		if len(approval.DingTalkApproval.ApprovalNodes) > 20 {
			return errors.New("num of approval-node should not exceed 20")
		}
		if len(approval.DingTalkApproval.ApprovalNodes) == 0 {
			return errors.New("num of approval-node is 0")
		}
		for i, node := range approval.DingTalkApproval.ApprovalNodes {
			if len(node.ApproveUsers) == 0 {
				return errors.Errorf("num of approval-node %d approver is 0", i)
			}
			for _, user := range node.ApproveUsers {
				if userIDSets.Has(user.ID) {
					return errors.Errorf("Duplicate approvers %s should not appear in a complete approval process", user.Name)
				}
				userIDSets.Insert(user.ID)
			}
			if !lo.Contains([]string{"AND", "OR"}, string(node.Type)) {
				return errors.Errorf("approval-node %d type should be AND or OR", i)
			}
		}
	default:
		return errors.Errorf("invalid approval type %s", approval.Type)
	}

	return nil
}

func createLarkApprovalDefinition(workflow *commonmodels.WorkflowV4) error {
	for _, stage := range workflow.Stages {
		if stage.Approval == nil {
			continue
		}
		if data := stage.Approval.LarkApproval; data != nil && data.ID != "" {
			larkInfo, err := commonrepo.NewIMAppColl().GetByID(context.Background(), stage.Approval.LarkApproval.ID)
			if err != nil {
				return errors.Wrapf(err, "get lark app %s", stage.Approval.LarkApproval.ID)
			}
			if larkInfo.Type != string(config.LarkApproval) {
				return errors.Errorf("lark app %s is not lark approval", stage.Approval.LarkApproval.ID)
			}

			if larkInfo.LarkApprovalCodeList == nil {
				larkInfo.LarkApprovalCodeList = make(map[string]string)
			}
			// skip if this node type approval definition already created
			if approvalNodeTypeID := larkInfo.LarkApprovalCodeList[data.GetNodeTypeKey()]; approvalNodeTypeID != "" {
				log.Infof("lark approval definition %s already created", approvalNodeTypeID)
				continue
			}

			// create this node type approval definition and save to db
			client, err := larkservice.GetLarkClientByIMAppID(data.ID)
			if err != nil {
				return errors.Wrapf(err, "get lark client by im app id %s", data.ID)
			}
			nodesArgs := make([]*lark.ApprovalNode, 0)
			for _, node := range data.ApprovalNodes {
				nodesArgs = append(nodesArgs, &lark.ApprovalNode{
					Type: node.Type,
					ApproverIDList: func() (re []string) {
						for _, user := range node.ApproveUsers {
							re = append(re, user.ID)
						}
						return
					}(),
				})
			}

			approvalCode, err := client.CreateApprovalDefinition(&lark.CreateApprovalDefinitionArgs{
				Name:        "Zadig 工作流",
				Description: "Zadig 工作流-" + data.GetNodeTypeKey(),
				Nodes:       nodesArgs,
			})
			if err != nil {
				return errors.Wrap(err, "create lark approval definition")
			}
			err = client.SubscribeApprovalDefinition(&lark.SubscribeApprovalDefinitionArgs{
				ApprovalID: approvalCode,
			})
			if err != nil {
				return errors.Wrap(err, "subscribe lark approval definition")
			}
			larkInfo.LarkApprovalCodeList[data.GetNodeTypeKey()] = approvalCode
			if err := commonrepo.NewIMAppColl().Update(context.Background(), stage.Approval.LarkApproval.ID, larkInfo); err != nil {
				return errors.Wrap(err, "update lark approval data")
			}
			log.Infof("create lark approval definition %s, key: %s", approvalCode, data.GetNodeTypeKey())
		}
	}
	return nil
}
func CreateWebhookForWorkflowV4(workflowName string, input *commonmodels.WorkflowV4Hook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(input.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrCreateWebhook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrCreateWebhook.AddErr(err)
	}
	for _, hook := range workflow.HookCtls {
		if hook.Name == input.Name {
			errMsg := fmt.Sprintf("webhook %s already exists", input.Name)
			logger.Error(errMsg)
			return e.ErrCreateWebhook.AddDesc(errMsg)
		}
	}
	if err := validateHookNames([]string{input.Name}); err != nil {
		logger.Errorf(err.Error())
		return e.ErrCreateWebhook.AddErr(err)
	}
	err = commonservice.ProcessWebhook([]*models.WorkflowV4Hook{input}, nil, webhook.WorkflowV4Prefix+workflowName, logger)
	if err != nil {
		errMsg := fmt.Sprintf("failed to create webhook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrCreateWebhook.AddDesc(errMsg)
	}
	workflow.HookCtls = append(workflow.HookCtls, input)
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to create webhook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrCreateWebhook.AddDesc(errMsg)
	}
	if !input.IsManual {
		if err := createGerritWebhook(input.MainRepo, workflowName); err != nil {
			logger.Errorf("create gerrit webhook failed: %v", err)
		}
	}
	return nil
}

func UpdateWebhookForWorkflowV4(workflowName string, input *commonmodels.WorkflowV4Hook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(input.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrUpdateWebhook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrUpdateWebhook.AddErr(err)
	}
	updatedHooks := []*commonmodels.WorkflowV4Hook{}
	var existHook *commonmodels.WorkflowV4Hook
	for _, hook := range workflow.HookCtls {
		if hook.Name == input.Name {
			updatedHooks = append(updatedHooks, input)
			existHook = hook
			continue
		}
		updatedHooks = append(updatedHooks, hook)
	}
	if existHook == nil {
		errMsg := fmt.Sprintf("webhook %s does not exist", input.Name)
		logger.Error(errMsg)
		return e.ErrUpdateWebhook.AddDesc(errMsg)
	}
	if err := validateHookNames([]string{input.Name}); err != nil {
		logger.Errorf(err.Error())
		return e.ErrUpdateWebhook.AddErr(err)
	}
	err = commonservice.ProcessWebhook([]*models.WorkflowV4Hook{input}, []*models.WorkflowV4Hook{existHook}, webhook.WorkflowV4Prefix+workflowName, logger)
	if err != nil {
		errMsg := fmt.Sprintf("failed to update webhook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrUpdateWebhook.AddDesc(errMsg)
	}
	workflow.HookCtls = updatedHooks
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to update webhook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrUpdateWebhook.AddDesc(errMsg)
	}

	if !existHook.IsManual {
		if err := deleteGerritWebhook(existHook.MainRepo, workflowName); err != nil {
			logger.Errorf("delete gerrit webhook failed: %v", err)
		}
	}
	if !input.IsManual {
		if err := createGerritWebhook(input.MainRepo, workflowName); err != nil {
			logger.Errorf("create gerrit webhook failed: %v", err)
		}
	}
	return nil
}

func ListWebhookForWorkflowV4(workflowName string, logger *zap.SugaredLogger) ([]*commonmodels.WorkflowV4Hook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return []*commonmodels.WorkflowV4Hook{}, e.ErrListWebhook.AddErr(err)
	}
	return workflow.HookCtls, nil
}

func GetWebhookForWorkflowV4Preset(workflowName, triggerName string, logger *zap.SugaredLogger) (*commonmodels.WorkflowV4Hook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrGetWebhook.AddErr(err)
	}
	var workflowArg *commonmodels.WorkflowV4
	workflowHook := &commonmodels.WorkflowV4Hook{}
	for _, hook := range workflow.HookCtls {
		if hook.Name == triggerName {
			workflowArg = hook.WorkflowArg
			workflowHook = hook
			break
		}
	}
	repos, err := job.GetRepos(workflow)
	if err != nil {
		errMsg := fmt.Sprintf("get workflow webhook repos error: %v", err)
		log.Error(errMsg)
		return nil, e.ErrGetWebhook.AddDesc(errMsg)
	}
	if err := job.MergeArgs(workflow, workflowArg); err != nil {
		errMsg := fmt.Sprintf("merge workflow args error: %v", err)
		log.Error(errMsg)
		return nil, e.ErrGetWebhook.AddDesc(errMsg)
	}
	workflowHook.Repos = repos
	workflowHook.WorkflowArg = workflow
	workflowHook.WorkflowArg.JiraHookCtls = nil
	workflowHook.WorkflowArg.MeegoHookCtls = nil
	workflowHook.WorkflowArg.GeneralHookCtls = nil
	workflowHook.WorkflowArg.HookCtls = nil
	return workflowHook, nil
}

func DeleteWebhookForWorkflowV4(workflowName, triggerName string, logger *zap.SugaredLogger) error {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrDeleteWebhook.AddErr(err)
	}
	updatedHooks := []*commonmodels.WorkflowV4Hook{}
	var existHook *commonmodels.WorkflowV4Hook
	for _, hook := range workflow.HookCtls {
		if hook.Name == triggerName {
			existHook = hook
			continue
		}
		updatedHooks = append(updatedHooks, hook)
	}
	if existHook == nil {
		errMsg := fmt.Sprintf("webhook %s does not exist", triggerName)
		logger.Error(errMsg)
		return e.ErrDeleteWebhook.AddDesc(errMsg)
	}
	err = commonservice.ProcessWebhook(nil, []*models.WorkflowV4Hook{existHook}, webhook.WorkflowV4Prefix+workflowName, logger)
	if err != nil {
		errMsg := fmt.Sprintf("failed to delete webhook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrDeleteWebhook.AddDesc(errMsg)
	}
	workflow.HookCtls = updatedHooks
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to delete webhook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrDeleteWebhook.AddDesc(errMsg)
	}
	if err := deleteGerritWebhook(existHook.MainRepo, workflowName); err != nil {
		logger.Errorf("delete gerrit webhook failed: %v", err)
	}
	return nil
}

func CreateGeneralHookForWorkflowV4(workflowName string, arg *models.GeneralHook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(arg.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrCreateGeneralHook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrCreateGeneralHook.AddErr(err)
	}
	for _, hook := range workflow.GeneralHookCtls {
		if hook.Name == arg.Name {
			errMsg := fmt.Sprintf("general hook %s already exists", arg.Name)
			logger.Error(errMsg)
			return e.ErrCreateGeneralHook.AddDesc(errMsg)
		}
	}
	if err := validateHookNames([]string{arg.Name}); err != nil {
		logger.Errorf(err.Error())
		return e.ErrCreateGeneralHook.AddErr(err)
	}
	workflow.GeneralHookCtls = append(workflow.GeneralHookCtls, arg)
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to create general hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrCreateGeneralHook.AddDesc(errMsg)
	}
	return nil
}

func GetGeneralHookForWorkflowV4Preset(workflowName, hookName string, logger *zap.SugaredLogger) (*commonmodels.GeneralHook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrGetGeneralHook.AddErr(err)
	}
	gHook := &commonmodels.GeneralHook{}
	for _, hook := range workflow.GeneralHookCtls {
		if hook.Name == hookName {
			gHook = hook
		}
	}
	if err := job.MergeArgs(workflow, gHook.WorkflowArg); err != nil {
		errMsg := fmt.Sprintf("merge workflow args error: %v", err)
		log.Error(errMsg)
		return nil, e.ErrGetGeneralHook.AddDesc(errMsg)
	}
	gHook.WorkflowArg = workflow
	gHook.WorkflowArg.JiraHookCtls = nil
	gHook.WorkflowArg.MeegoHookCtls = nil
	gHook.WorkflowArg.GeneralHookCtls = nil
	gHook.WorkflowArg.HookCtls = nil
	return gHook, nil
}

func ListGeneralHookForWorkflowV4(workflowName string, logger *zap.SugaredLogger) ([]*models.GeneralHook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrListGeneralHook.AddErr(err)
	}
	return workflow.GeneralHookCtls, nil
}

func UpdateGeneralHookForWorkflowV4(workflowName string, arg *models.GeneralHook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(arg.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrUpdateGeneralHook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrUpdateGeneralHook.AddErr(err)
	}
	updated := false
	for i, hook := range workflow.GeneralHookCtls {
		if hook.Name == arg.Name {
			workflow.GeneralHookCtls[i] = arg
			updated = true
		}
	}
	if !updated {
		errMsg := fmt.Sprintf("failed to find general hook %s", arg.Name)
		log.Error(errMsg)
		return e.ErrUpdateGeneralHook.AddDesc(errMsg)
	}
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to update general hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrUpdateGeneralHook.AddDesc(errMsg)
	}
	return nil
}

func DeleteGeneralHookForWorkflowV4(workflowName, hookName string, logger *zap.SugaredLogger) error {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrDeleteGeneralHook.AddErr(err)
	}
	var list []*models.GeneralHook
	for _, ctl := range workflow.GeneralHookCtls {
		if ctl.Name == hookName {
			continue
		}
		list = append(list, ctl)
	}
	if len(list) == len(workflow.GeneralHookCtls) {
		errMsg := fmt.Sprintf("general hook %s not found", hookName)
		log.Error(errMsg)
		return e.ErrDeleteGeneralHook.AddDesc(errMsg)
	}
	workflow.GeneralHookCtls = list
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to delete general hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrDeleteGeneralHook.AddDesc(errMsg)
	}
	return nil
}

func GeneralHookEventHandler(workflowName, hookName string, logger *zap.SugaredLogger) error {
	workflowInfo, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		logger.Error(errMsg)
		return errors.New(errMsg)
	}
	var generalHook *models.GeneralHook
	for _, hook := range workflowInfo.GeneralHookCtls {
		if hook.Name == hookName {
			generalHook = hook
			break
		}
	}
	if generalHook == nil {
		errMsg := fmt.Sprintf("Failed to find general hook %s", hookName)
		logger.Error(errMsg)
		return errors.New(errMsg)
	}
	if !generalHook.Enabled {
		errMsg := fmt.Sprintf("Not enabled general hook %s", hookName)
		logger.Error(errMsg)
		return errors.New(errMsg)
	}
	_, err = CreateWorkflowTaskV4ByBuildInTrigger(setting.GeneralHookTaskCreator, generalHook.WorkflowArg, logger)
	if err != nil {
		errMsg := fmt.Sprintf("HandleGeneralHookEvent: failed to create workflow task: %s", err)
		logger.Error(errMsg)
		return errors.New(errMsg)
	}
	logger.Infof("HandleGeneralHookEvent: workflow-%s hook-%s create workflow task success", workflowName, hookName)
	return nil
}

func CreateJiraHookForWorkflowV4(workflowName string, arg *models.JiraHook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(arg.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrCreateJiraHook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrCreateJiraHook.AddErr(err)
	}
	for _, hook := range workflow.JiraHookCtls {
		if hook.Name == arg.Name {
			errMsg := fmt.Sprintf("jira hook %s already exists", arg.Name)
			logger.Error(errMsg)
			return e.ErrCreateJiraHook.AddDesc(errMsg)
		}
	}
	if err := validateHookNames([]string{arg.Name}); err != nil {
		logger.Errorf(err.Error())
		return e.ErrCreateJiraHook.AddErr(err)
	}
	workflow.JiraHookCtls = append(workflow.JiraHookCtls, arg)
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to create jira hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrCreateJiraHook.AddDesc(errMsg)
	}
	return nil
}

func GetJiraHookForWorkflowV4Preset(workflowName, hookName string, logger *zap.SugaredLogger) (*commonmodels.JiraHook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrGetJiraHook.AddErr(err)
	}
	jiraHook := &commonmodels.JiraHook{}
	for _, hook := range workflow.JiraHookCtls {
		if hook.Name == hookName {
			jiraHook = hook
		}
	}
	if err := job.MergeArgs(workflow, jiraHook.WorkflowArg); err != nil {
		errMsg := fmt.Sprintf("merge workflow args error: %v", err)
		log.Error(errMsg)
		return nil, e.ErrGetJiraHook.AddDesc(errMsg)
	}
	jiraHook.WorkflowArg = workflow
	jiraHook.WorkflowArg.JiraHookCtls = nil
	jiraHook.WorkflowArg.MeegoHookCtls = nil
	jiraHook.WorkflowArg.GeneralHookCtls = nil
	jiraHook.WorkflowArg.HookCtls = nil
	return jiraHook, nil
}

func ListJiraHookForWorkflowV4(workflowName string, logger *zap.SugaredLogger) ([]*models.JiraHook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrListJiraHook.AddErr(err)
	}
	return workflow.JiraHookCtls, nil
}

func UpdateJiraHookForWorkflowV4(workflowName string, arg *models.JiraHook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(arg.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrUpdateJiraHook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrUpdateJiraHook.AddErr(err)
	}
	updated := false
	for i, hook := range workflow.JiraHookCtls {
		if hook.Name == arg.Name {
			workflow.JiraHookCtls[i] = arg
			updated = true
		}
	}
	if !updated {
		errMsg := fmt.Sprintf("failed to find jira hook %s", arg.Name)
		log.Error(errMsg)
		return e.ErrUpdateJiraHook.AddDesc(errMsg)
	}
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to update jira hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrUpdateJiraHook.AddDesc(errMsg)
	}
	return nil
}

func DeleteJiraHookForWorkflowV4(workflowName, hookName string, logger *zap.SugaredLogger) error {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrDeleteJiraHook.AddErr(err)
	}
	var list []*models.JiraHook
	for _, ctl := range workflow.JiraHookCtls {
		if ctl.Name == hookName {
			continue
		}
		list = append(list, ctl)
	}
	if len(list) == len(workflow.JiraHookCtls) {
		errMsg := fmt.Sprintf("jira hook %s not found", hookName)
		log.Error(errMsg)
		return e.ErrDeleteJiraHook.AddDesc(errMsg)
	}
	workflow.JiraHookCtls = list
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to delete jira hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrDeleteJiraHook.AddDesc(errMsg)
	}
	return nil
}

func CreateMeegoHookForWorkflowV4(workflowName string, arg *models.MeegoHook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(arg.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrCreateMeegoHook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrCreateMeegoHook.AddErr(err)
	}
	for _, hook := range workflow.MeegoHookCtls {
		if hook.Name == arg.Name {
			errMsg := fmt.Sprintf("meego hook %s already exists", arg.Name)
			logger.Error(errMsg)
			return e.ErrCreateMeegoHook.AddDesc(errMsg)
		}
	}
	if err := validateHookNames([]string{arg.Name}); err != nil {
		logger.Errorf(err.Error())
		return e.ErrCreateMeegoHook.AddErr(err)
	}
	workflow.MeegoHookCtls = append(workflow.MeegoHookCtls, arg)
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to create jira hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrCreateMeegoHook.AddDesc(errMsg)
	}
	return nil
}

func GetMeegoHookForWorkflowV4Preset(workflowName, hookName string, logger *zap.SugaredLogger) (*commonmodels.MeegoHook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrGetMeegoHook.AddErr(err)
	}
	meegoHook := &commonmodels.MeegoHook{}
	for _, hook := range workflow.MeegoHookCtls {
		if hook.Name == hookName {
			meegoHook = hook
		}
	}
	if err := job.MergeArgs(workflow, meegoHook.WorkflowArg); err != nil {
		errMsg := fmt.Sprintf("merge workflow args error: %v", err)
		log.Error(errMsg)
		return nil, e.ErrGetMeegoHook.AddDesc(errMsg)
	}
	meegoHook.WorkflowArg = workflow
	meegoHook.WorkflowArg.JiraHookCtls = nil
	meegoHook.WorkflowArg.MeegoHookCtls = nil
	meegoHook.WorkflowArg.GeneralHookCtls = nil
	meegoHook.WorkflowArg.HookCtls = nil
	return meegoHook, nil
}

func ListMeegoHookForWorkflowV4(workflowName string, logger *zap.SugaredLogger) ([]*models.MeegoHook, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrListMeegoHook.AddErr(err)
	}
	return workflow.MeegoHookCtls, nil
}

func UpdateMeegoHookForWorkflowV4(workflowName string, arg *models.MeegoHook, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(arg.WorkflowArg); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrUpdateMeegoHook.AddErr(err)
	}

	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrUpdateMeegoHook.AddErr(err)
	}
	updated := false
	for i, hook := range workflow.MeegoHookCtls {
		if hook.Name == arg.Name {
			workflow.MeegoHookCtls[i] = arg
			updated = true
		}
	}
	if !updated {
		errMsg := fmt.Sprintf("failed to find jira hook %s", arg.Name)
		log.Error(errMsg)
		return e.ErrUpdateMeegoHook.AddDesc(errMsg)
	}
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to update jira hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrUpdateMeegoHook.AddDesc(errMsg)
	}
	return nil
}

func DeleteMeegoHookForWorkflowV4(workflowName, hookName string, logger *zap.SugaredLogger) error {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return e.ErrDeleteMeegoHook.AddErr(err)
	}
	var list []*models.MeegoHook
	for _, ctl := range workflow.MeegoHookCtls {
		if ctl.Name == hookName {
			continue
		}
		list = append(list, ctl)
	}
	if len(list) == len(workflow.MeegoHookCtls) {
		errMsg := fmt.Sprintf("meego hook %s not found", hookName)
		log.Error(errMsg)
		return e.ErrDeleteMeegoHook.AddDesc(errMsg)
	}
	workflow.MeegoHookCtls = list
	if err := commonrepo.NewWorkflowV4Coll().Update(workflow.ID.Hex(), workflow); err != nil {
		errMsg := fmt.Sprintf("failed to delete jira hook for workflow %s, the error is: %v", workflowName, err)
		log.Error(errMsg)
		return e.ErrDeleteMeegoHook.AddDesc(errMsg)
	}
	return nil
}

func BulkCopyWorkflowV4(args BulkCopyWorkflowArgs, username string, log *zap.SugaredLogger) error {
	var workflows []commonrepo.WorkflowV4

	for _, item := range args.Items {
		workflows = append(workflows, commonrepo.WorkflowV4{
			ProjectName: item.ProjectName,
			Name:        item.Old,
		})
	}
	oldWorkflows, err := commonrepo.NewWorkflowV4Coll().ListByWorkflows(commonrepo.ListWorkflowV4Opt{
		Workflows: workflows,
	})
	if err != nil {
		log.Error(err)
		return e.ErrGetPipeline.AddErr(err)
	}
	workflowMap := make(map[string]*commonmodels.WorkflowV4)
	for _, workflow := range oldWorkflows {
		workflowMap[workflow.Project+"-"+workflow.Name] = workflow
	}
	var newWorkflows []*commonmodels.WorkflowV4
	for _, workflow := range args.Items {
		if item, ok := workflowMap[workflow.ProjectName+"-"+workflow.Old]; ok {
			newItem := *item
			newItem.UpdatedBy = username
			newItem.Name = workflow.New
			newItem.DisplayName = workflow.NewDisplayName
			newItem.BaseName = workflow.BaseName
			newItem.ID = primitive.NewObjectID()
			// do not copy webhook triggers.
			newItem.HookCtls = []*commonmodels.WorkflowV4Hook{}

			newWorkflows = append(newWorkflows, &newItem)
		} else {
			return fmt.Errorf("workflow:%s not exist", item.Project+"-"+item.Name)
		}
	}
	return commonrepo.NewWorkflowV4Coll().BulkCreate(newWorkflows)
}

func CreateCronForWorkflowV4(workflowName string, input *commonmodels.Cronjob, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(input.WorkflowV4Args); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrUpsertCronjob.AddErr(err)
	}

	if !input.ID.IsZero() {
		return e.ErrUpsertCronjob.AddDesc("cronjob id is not empty")
	}
	input.Name = workflowName
	input.Type = config.WorkflowV4Cronjob
	err := commonrepo.NewCronjobColl().Create(input)
	if err != nil {
		msg := fmt.Sprintf("Failed to create cron job, error: %v", err)
		log.Error(msg)
		return errors.New(msg)
	}
	if !input.Enabled {
		return nil
	}

	payload := &commonservice.CronjobPayload{
		Name:    workflowName,
		JobType: config.WorkflowV4Cronjob,
		Action:  setting.TypeEnableCronjob,
		JobList: []*commonmodels.Schedule{cronJobToSchedule(input)},
	}

	pl, _ := json.Marshal(payload)
	err = commonrepo.NewMsgQueueCommonColl().Create(&msg_queue.MsgQueueCommon{
		Payload:   string(pl),
		QueueType: setting.TopicCronjob,
	})
	if err != nil {
		log.Errorf("Failed to publish cron to MsgQueueCommon, the error is: %v", err)
		return e.ErrUpsertCronjob.AddDesc(err.Error())
	}
	return nil
}

func UpdateCronForWorkflowV4(input *commonmodels.Cronjob, logger *zap.SugaredLogger) error {
	if err := jobctl.InstantiateWorkflow(input.WorkflowV4Args); err != nil {
		logger.Errorf("instantiate hook args error: %s", err)
		return e.ErrUpsertCronjob.AddErr(err)
	}

	_, err := commonrepo.NewCronjobColl().GetByID(input.ID)
	if err != nil {
		msg := fmt.Sprintf("cron job not exist, error: %v", err)
		log.Error(msg)
		return errors.New(msg)
	}
	if err := commonrepo.NewCronjobColl().Update(input); err != nil {
		msg := fmt.Sprintf("Failed to update cron job, error: %v", err)
		log.Error(msg)
		return errors.New(msg)
	}
	payload := &commonservice.CronjobPayload{
		Name:    input.Name,
		JobType: config.WorkflowV4Cronjob,
		Action:  setting.TypeEnableCronjob,
	}
	if !input.Enabled {
		payload.DeleteList = []string{input.ID.Hex()}
	} else {
		payload.JobList = []*commonmodels.Schedule{cronJobToSchedule(input)}
	}

	pl, _ := json.Marshal(payload)
	err = commonrepo.NewMsgQueueCommonColl().Create(&msg_queue.MsgQueueCommon{
		Payload:   string(pl),
		QueueType: setting.TopicCronjob,
	})
	if err != nil {
		log.Errorf("Failed to publish cron to MsgQueueCommon, the error is: %v", err)
		return e.ErrUpsertCronjob.AddDesc(err.Error())
	}
	return nil
}

func ListCronForWorkflowV4(workflowName string, logger *zap.SugaredLogger) ([]*commonmodels.Cronjob, error) {
	crons, err := commonrepo.NewCronjobColl().List(&commonrepo.ListCronjobParam{
		ParentName: workflowName,
		ParentType: config.WorkflowV4Cronjob,
	})
	if err != nil {
		logger.Errorf("Failed to list WorkflowV4 : %s cron jobs, the error is: %v", workflowName, err)
		return crons, e.ErrUpsertCronjob.AddErr(err)
	}
	return crons, nil
}

func GetCronForWorkflowV4Preset(workflowName, cronID string, logger *zap.SugaredLogger) (*commonmodels.Cronjob, error) {
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		logger.Errorf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		return nil, e.ErrUpsertCronjob.AddErr(err)
	}
	cronJob := &commonmodels.Cronjob{}
	if cronID != "" {
		id, err := primitive.ObjectIDFromHex(cronID)
		if err != nil {
			logger.Errorf("Failed to parse cron id: %s, the error is: %v", cronID, err)
			return nil, e.ErrUpsertCronjob.AddErr(err)
		}
		cronJob, err = commonrepo.NewCronjobColl().GetByID(id)
		if err != nil {
			msg := fmt.Sprintf("cron job not exist, error: %v", err)
			log.Error(msg)
			return nil, errors.New(msg)
		}
	}

	if err := job.MergeArgs(workflow, cronJob.WorkflowV4Args); err != nil {
		errMsg := fmt.Sprintf("merge workflow args error: %v", err)
		log.Error(errMsg)
		return nil, e.ErrGetWebhook.AddDesc(errMsg)
	}
	cronJob.WorkflowV4Args = workflow
	return cronJob, nil
}

func DeleteCronForWorkflowV4(workflowName, cronID string, logger *zap.SugaredLogger) error {
	id, err := primitive.ObjectIDFromHex(cronID)
	if err != nil {
		logger.Errorf("Failed to parse cron id: %s, the error is: %v", cronID, err)
		return e.ErrUpsertCronjob.AddErr(err)
	}
	job, err := commonrepo.NewCronjobColl().GetByID(id)
	if err != nil {
		msg := fmt.Sprintf("cron job not exist, error: %v", err)
		log.Error(msg)
		return errors.New(msg)
	}
	payload := &commonservice.CronjobPayload{
		Name:       workflowName,
		JobType:    config.WorkflowV4Cronjob,
		Action:     setting.TypeEnableCronjob,
		DeleteList: []string{job.ID.Hex()},
	}

	pl, _ := json.Marshal(payload)
	err = commonrepo.NewMsgQueueCommonColl().Create(&msg_queue.MsgQueueCommon{
		Payload:   string(pl),
		QueueType: setting.TopicCronjob,
	})
	if err != nil {
		log.Errorf("Failed to publish cron to MsgQueueCommon, the error is: %v", err)
		return e.ErrUpsertCronjob.AddDesc(err.Error())
	}
	if err := commonrepo.NewCronjobColl().Delete(&commonrepo.CronjobDeleteOption{IDList: []string{cronID}}); err != nil {
		logger.Errorf("Failed to delete cron job: %s, the error is: %v", cronID, err)
		return e.ErrUpsertCronjob.AddDesc(err.Error())
	}
	return nil
}

func cronJobToSchedule(input *commonmodels.Cronjob) *commonmodels.Schedule {
	return &commonmodels.Schedule{
		ID:             input.ID,
		Number:         input.Number,
		Frequency:      input.Frequency,
		Time:           input.Time,
		MaxFailures:    input.MaxFailure,
		WorkflowV4Args: input.WorkflowV4Args,
		Type:           config.ScheduleType(input.JobType),
		Cron:           input.Cron,
		Enabled:        input.Enabled,
	}
}

func GetPatchParams(patchItem *commonmodels.PatchItem, logger *zap.SugaredLogger) ([]*commonmodels.Param, error) {
	resp := []*commonmodels.Param{}
	kvs, err := commomtemplate.GetYamlVariables(patchItem.PatchContent, logger)
	if err != nil {
		return resp, fmt.Errorf("get kv from content error: %s", err)
	}
	paramMap := map[string]*commonmodels.Param{}
	for _, param := range patchItem.Params {
		paramMap[param.Name] = param
	}
	for _, kv := range kvs {
		if param, ok := paramMap[kv.Key]; ok {
			resp = append(resp, param)
			continue
		}
		resp = append(resp, &commonmodels.Param{
			Name:       kv.Key,
			ParamsType: "string",
		})
	}
	return resp, nil
}

func GetWorkflowRepoIndex(workflow *commonmodels.WorkflowV4, currentJobName string, log *zap.SugaredLogger) []*jobctl.RepoIndex {
	return jobctl.GetWorkflowRepoIndex(workflow, currentJobName, log)
}

func GetWorkflowGlabalVars(workflow *commonmodels.WorkflowV4, currentJobName string, log *zap.SugaredLogger) []string {
	return append(getDefaultVars(workflow, currentJobName), jobctl.GetWorkflowOutputs(workflow, currentJobName, log)...)
}

func getDefaultVars(workflow *commonmodels.WorkflowV4, currentJobName string) []string {
	vars := []string{}
	vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, "project"))
	vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, "workflow.name"))
	vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, "workflow.task.creator"))
	vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, "workflow.task.creator.id"))
	vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, "workflow.task.timestamp"))
	vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, "workflow.task.id"))
	for _, param := range workflow.Params {
		if param.ParamsType == "repo" {
			continue
		}
		vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"workflow", "params", param.Name}, ".")))
	}
	for _, stage := range workflow.Stages {
		for _, j := range stage.Jobs {
			if j.Name == currentJobName {
				continue
			}
			switch j.JobType {
			case config.JobZadigBuild:
				spec := new(commonmodels.ZadigBuildJobSpec)
				if err := commonmodels.IToiYaml(j.Spec, spec); err != nil {
					return vars
				}
				vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, "SERVICES"}, ".")))
				vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, "BRANCHES"}, ".")))
				vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, "IMAGES"}, ".")))
				vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, "GITURLS"}, ".")))
				for _, s := range spec.ServiceAndBuilds {
					vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, s.ServiceName, s.ServiceModule, "COMMITID"}, ".")))
					vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, s.ServiceName, s.ServiceModule, "BRANCH"}, ".")))
				}
			case config.JobZadigDeploy:
				vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, "envName"}, ".")))
				vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, "IMAGES"}, ".")))
			case config.JobZadigDistributeImage:
				vars = append(vars, fmt.Sprintf(setting.RenderValueTemplate, strings.Join([]string{"job", j.Name, "IMAGES"}, ".")))
			}
		}
	}
	return vars
}

func CheckShareStorageEnabled(clusterID, jobType, identifyName, project string, logger *zap.SugaredLogger) (bool, error) {
	// if cluster id was set, we just check if the cluster has share storage enabled
	if clusterID != "" {
		return checkClusterShareStorage(clusterID)
	}
	switch jobType {
	case string(config.JobZadigBuild):
		build, err := commonrepo.NewBuildColl().Find(&commonrepo.BuildFindOption{Name: identifyName, ProductName: project})
		if err != nil {
			return false, fmt.Errorf("find build error: %v", err)
		}
		if build.TemplateID == "" {
			clusterID = build.PreBuild.ClusterID
			break
		}
		template, err := commonrepo.NewBuildTemplateColl().Find(&commonrepo.BuildTemplateQueryOption{ID: build.TemplateID})
		if err != nil {
			return false, fmt.Errorf("find build template error: %v", err)
		}
		clusterID = template.PreBuild.ClusterID
	case string(config.JobZadigTesting):
		testing, err := commonrepo.NewTestingColl().Find(identifyName, "")
		if err != nil {
			return false, fmt.Errorf("find testing error: %v", err)
		}
		clusterID = testing.PreTest.ClusterID
	case string(config.JobZadigScanning):
		scanning, err := commonrepo.NewScanningColl().Find(project, identifyName)
		if err != nil {
			return false, fmt.Errorf("find scanning error: %v", err)
		}
		clusterID = scanning.AdvancedSetting.ClusterID
	default:
		return false, fmt.Errorf("job type %s is not supported", jobType)
	}
	if clusterID == "" {
		clusterID = setting.LocalClusterID
	}
	return checkClusterShareStorage(clusterID)
}

func checkClusterShareStorage(id string) (bool, error) {
	cluster, err := commonrepo.NewK8SClusterColl().Get(id)
	if err != nil {
		return false, fmt.Errorf("find cluter error: %v", err)
	}
	if cluster.ShareStorage.NFSProperties.PVC != "" {
		return true, nil
	}
	return false, nil
}

func ListAllAvailableWorkflows(projects []string, log *zap.SugaredLogger) ([]*Workflow, error) {
	resp := make([]*Workflow, 0)
	allProductWorkflows, err := commonrepo.NewWorkflowColl().ListWorkflowsByProjects(projects)
	if err != nil {
		log.Errorf("failed to get all product workflows, error: %s", err)
		return nil, err
	}
	for _, productWorkflow := range allProductWorkflows {
		resp = append(resp, &Workflow{
			Name:         productWorkflow.Name,
			DisplayName:  productWorkflow.DisplayName,
			ProjectName:  productWorkflow.ProductTmplName,
			UpdateTime:   productWorkflow.UpdateTime,
			CreateTime:   productWorkflow.CreateTime,
			UpdateBy:     productWorkflow.UpdateBy,
			WorkflowType: "",
			Description:  productWorkflow.Description,
			BaseName:     productWorkflow.BaseName,
		})
	}

	allCustomWorkflows, err := commonrepo.NewWorkflowV4Coll().ListByProjectNames(projects)
	if err != nil {
		log.Errorf("failed to get all custom workflows, error: %s", err)
		return nil, err
	}

	for _, customWorkflow := range allCustomWorkflows {
		resp = append(resp, &Workflow{
			Name:         customWorkflow.Name,
			DisplayName:  customWorkflow.DisplayName,
			ProjectName:  customWorkflow.Project,
			UpdateTime:   customWorkflow.UpdateTime,
			CreateTime:   customWorkflow.CreateTime,
			UpdateBy:     customWorkflow.UpdatedBy,
			WorkflowType: setting.CustomWorkflowType,
			Description:  customWorkflow.Description,
			BaseName:     customWorkflow.BaseName,
		})
	}

	return resp, nil
}

func GetLatestTaskInfo(workflowInfo *Workflow) (startTime int64, creator, status string) {
	// if we found it is a custom workflow, search it in the custom workflow task
	if workflowInfo.WorkflowType == setting.CustomWorkflowType {
		taskInfo, err := getLatestWorkflowTaskV4(workflowInfo.Name)
		if err != nil {
			return 0, "", ""
		}
		return taskInfo.StartTime, taskInfo.TaskCreator, string(taskInfo.Status)
	} else {
		// otherwise it is a product workflow
		taskInfo, err := getLatestWorkflowTask(workflowInfo.Name)
		if err != nil {
			return 0, "", ""
		}
		return taskInfo.StartTime, taskInfo.TaskCreator, string(taskInfo.Status)
	}
}

func GetFilteredEnvServices(workflowName, jobName, envName string, serviceNames []string, log *zap.SugaredLogger) ([]*commonmodels.DeployService, error) {
	resp := []*commonmodels.DeployService{}
	workflow, err := commonrepo.NewWorkflowV4Coll().Find(workflowName)
	if err != nil {
		msg := fmt.Sprintf("Failed to find WorkflowV4: %s, the error is: %v", workflowName, err)
		log.Error(msg)
		return resp, e.ErrFilterWorkflowVars.AddDesc(msg)
	}
	jobSpec := &commonmodels.ZadigDeployJobSpec{}
	found := false
	svcKVsMap := map[string][]*commonmodels.ServiceKeyVal{}
	for _, stage := range workflow.Stages {
		for _, job := range stage.Jobs {
			if job.Name != jobName {
				continue
			}
			if job.JobType != config.JobZadigDeploy {
				msg := fmt.Sprintf("job: %s is not a deploy job", jobName)
				log.Error(msg)
				return resp, e.ErrFilterWorkflowVars.AddDesc(msg)
			}
			if err := commonmodels.IToiYaml(job.Spec, jobSpec); err != nil {
				msg := fmt.Sprintf("unmarshal deploy job spec error: %v", err)
				log.Error(msg)
				return resp, e.ErrFilterWorkflowVars.AddDesc(msg)
			}

			for _, svc := range jobSpec.Services {
				svcKVsMap[svc.ServiceName] = svc.KeyVals
			}

			found = true
			break
		}
	}
	if !found {
		msg := fmt.Sprintf("job: %s not found", jobName)
		log.Error(msg)
		return resp, e.ErrFilterWorkflowVars.AddDesc(msg)
	}
	services, err := commonservice.ListServicesInEnv(envName, workflow.Project, svcKVsMap, log)
	if err != nil {
		return resp, e.ErrFilterWorkflowVars.AddErr(err)
	}

	serviceMap := map[string]*commonservice.EnvService{}
	for _, service := range services.Services {
		serviceMap[service.ServiceName] = service
	}
	deployServiceMap := map[string]*commonmodels.DeployService{}
	for _, service := range jobSpec.Services {
		deployServiceMap[service.ServiceName] = service
	}

	for _, serviceName := range serviceNames {
		service, err := filterServiceVars(serviceName, jobSpec.DeployContents, deployServiceMap[serviceName], serviceMap[serviceName])
		if err != nil {
			log.Error(err)
			return resp, e.ErrFilterWorkflowVars.AddErr(err)
		}
		resp = append(resp, service)
	}
	return resp, nil
}

func CompareHelmServiceYamlInEnv(serviceName, variableYaml, envName, projectName string, images []string, isProduction, updateServiceRevision, isHelmChartDeploy bool, log *zap.SugaredLogger) (*GetHelmValuesDifferenceResp, error) {
	opt := &commonrepo.ProductFindOptions{Name: projectName, EnvName: envName}
	prod, err := commonrepo.NewProductColl().Find(opt)
	if err != nil {
		return nil, fmt.Errorf("failed to find project: %s, err: %s", projectName, err)
	}

	if isHelmChartDeploy {
		renderSet, err := commonrepo.NewRenderSetColl().Find(&commonrepo.RenderSetFindOption{
			Name:        prod.Render.Name,
			ProductTmpl: prod.Render.ProductTmpl,
			Revision:    prod.Render.Revision,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to find render set name: %s, revision: %s, err: %s", prod.Render.Name, prod.Render.ProductTmpl, err)
		}

		currentYaml := ""
		for _, chartInfo := range renderSet.ChartInfos {
			if chartInfo.IsHelmChartDeploy && chartInfo.ReleaseName == serviceName {
				currentYaml, err = helmtool.MergeOverrideValues("", "", chartInfo.GetOverrideYaml(), chartInfo.OverrideValues, nil)
				if err != nil {
					return nil, fmt.Errorf("failed to merge override values, err: %s", err)
				}
				break
			}
		}
		return &GetHelmValuesDifferenceResp{
			Current: currentYaml,
			Latest:  variableYaml,
		}, nil
	}

	// first we get the current yaml in the current environment
	currentYaml := ""
	resp, err := commonservice.GetChartValues(projectName, envName, serviceName, false, isProduction)
	if err != nil {
		log.Infof("failed to get the current service[%s] values from project: %s, env: %s", serviceName, projectName, envName)
		currentYaml = ""
	} else {
		currentYaml = resp.ValuesYaml
	}

	param := &kube.ResourceApplyParam{
		ProductInfo: prod,
		ServiceName: serviceName,
		// no image preview available in this api
		Images:                images,
		Uninstall:             false,
		UpdateServiceRevision: updateServiceRevision,
		Timeout:               setting.DeployTimeout,
	}

	curProdSvcRevision := func() int64 {
		if prod.GetServiceMap()[serviceName] != nil {
			return prod.GetServiceMap()[serviceName].Revision
		}
		return 0
	}()

	renderSet, productService, _, err := kube.PrepareHelmServiceData(param)
	if err != nil {
		log.Errorf("prepare helm service data error: %v", err)
		return nil, err
	}

	if updateServiceRevision && curProdSvcRevision > int64(0) {
		svcFindOption := &commonrepo.ServiceFindOption{
			ProductName: prod.ProductName,
			ServiceName: serviceName,
		}
		latestSvc, err := repository.QueryTemplateService(svcFindOption, prod.Production)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to find service %s/%d in product %s", serviceName, svcFindOption.Revision, prod.ProductName)
		}

		curUsedSvc, err := repository.QueryTemplateService(&commonrepo.ServiceFindOption{
			ProductName: prod.ProductName,
			ServiceName: serviceName,
			Revision:    curProdSvcRevision,
		}, prod.Production)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to find service %s/%d in product %s", serviceName, svcFindOption.Revision, prod.ProductName)
		}

		containers := kube.CalculateContainer(productService, curUsedSvc, latestSvc.Containers, prod)
		images = kube.MergeImages(containers, images)
		param.Images = images
	}

	chartInfo, ok := renderSet.GetChartRenderMap()[param.ServiceName]
	if !ok {
		log.Errorf("failed to find chart info in render")
		return nil, err
	}
	if chartInfo.OverrideYaml == nil {
		chartInfo.OverrideYaml = &template.CustomYaml{}
	}

	param.VariableYaml = variableYaml
	chartInfo.OverrideYaml.YamlContent = variableYaml

	yamlContent, err := kube.GeneMergedValues(productService, renderSet, param.Images, true)
	if err != nil {
		log.Errorf("failed to generate merged values.yaml, err: %s", err)
		return nil, err
	}

	tmp := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(yamlContent), &tmp); err != nil {
		log.Errorf("failed to unmarshal latest yaml content, err: %s", err)
		return nil, err
	}

	// re-marshal it into string to make sure indentation is right
	latestYamlContent, err := yaml.Marshal(tmp)
	if err != nil {
		log.Errorf("failed to marshal latest yaml content, err: %s", err)
		return nil, err
	}
	return &GetHelmValuesDifferenceResp{
		Current: currentYaml,
		Latest:  string(latestYamlContent),
	}, nil
}

func GetMseOriginalServiceYaml(project, envName, serviceName, grayTag string) (string, error) {
	yamlContent, _, err := kube.FetchCurrentAppliedYaml(&kube.GeneSvcYamlOption{
		ProductName:           project,
		EnvName:               envName,
		ServiceName:           serviceName,
		UpdateServiceRevision: false,
	})
	if err != nil {
		return "", errors.Wrap(err, "failed to fetch current applied yaml")
	}

	var yamls []string
	resources := make([]*unstructured.Unstructured, 0)
	manifests := releaseutil.SplitManifests(yamlContent)
	for _, item := range manifests {
		u, err := serializer.NewDecoder().YamlToUnstructured([]byte(item))
		if err != nil {
			return "", errors.Errorf("failed to decode service %s yaml to unstructured: %v", serviceName, err)
		}
		resources = append(resources, u)
	}
	deploymentNum := 0
	nameSuffix := "-mse-" + grayTag
	for _, resource := range resources {
		switch resource.GetKind() {
		case setting.Deployment:
			if deploymentNum > 0 {
				return "", errors.Errorf("service-%s: only one deployment is allowed in each service", serviceName)
			}
			deploymentNum++

			deploymentObj := &v1.Deployment{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, deploymentObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s deployment to deployment object: %v", serviceName, err)
			}
			if deploymentObj.Spec.Selector == nil || !checkMapKeyExist(deploymentObj.Spec.Selector.MatchLabels, types.ZadigReleaseVersionLabelKey) {
				return "", errors.Errorf("service %s deployment label selector must contain %s", serviceName, types.ZadigReleaseVersionLabelKey)
			}
			if !checkMapKeyExist(deploymentObj.Spec.Template.Labels, types.ZadigReleaseVersionLabelKey) {
				return "", errors.Errorf("service %s deployment template label must contain %s", serviceName, types.ZadigReleaseVersionLabelKey)
			}
			deploymentObj.Name += nameSuffix
			deploymentObj.Spec.Replicas = pointer.Int32(1)
			deploymentObj.Labels = setMseLabels(deploymentObj.Labels, grayTag, serviceName)
			deploymentObj.Spec.Selector.MatchLabels = setMseDeploymentLabels(deploymentObj.Spec.Selector.MatchLabels, grayTag, serviceName)
			deploymentObj.Spec.Template.Labels = setMseDeploymentLabels(deploymentObj.Spec.Template.Labels, grayTag, serviceName)
			resp, err := toYaml(deploymentObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s deployment object: %v", serviceName, err)
			}
			yamls = append(yamls, resp)
		case setting.ConfigMap:
			cmObj := &corev1.ConfigMap{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, cmObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s ConfigMap to object: %v", serviceName, err)
			}
			cmObj.Name += nameSuffix
			cmObj.Labels = setMseLabels(cmObj.Labels, grayTag, serviceName)
			s, err := toYaml(cmObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s configmap object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		case setting.Service:
			serviceObj := &corev1.Service{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, serviceObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s Service to object: %v", serviceName, err)
			}
			serviceObj.Name += nameSuffix
			serviceObj.Labels = setMseLabels(serviceObj.Labels, grayTag, serviceName)
			serviceObj.Spec.Selector = setMseLabels(serviceObj.Spec.Selector, grayTag, serviceName)
			s, err := toYaml(serviceObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s service object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		case setting.Secret:
			secretObj := &corev1.Secret{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, secretObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s Secret to object: %v", serviceName, err)
			}
			secretObj.Name += nameSuffix
			secretObj.Labels = setMseLabels(secretObj.Labels, grayTag, serviceName)
			s, err := toYaml(secretObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s secret object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		case setting.Ingress:
			ingressObj := &networkingv1.Ingress{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, ingressObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s Ingress to object: %v", serviceName, err)
			}
			ingressObj.Name += nameSuffix
			ingressObj.Labels = setMseLabels(ingressObj.Labels, grayTag, serviceName)
			s, err := toYaml(ingressObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s ingress object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		default:
			return "", errors.Errorf("service %s resource type %s not allowed", serviceName, resource.GetKind())
		}
	}
	if deploymentNum == 0 {
		return "", errors.Errorf("service %s must contain one deployment", serviceName)
	}
	return strings.Join(yamls, "---\n"), nil
}

func RenderMseServiceYaml(productName, envName, lastGrayTag, grayTag string, service *commonmodels.MseGrayReleaseService) (string, error) {
	resources := make([]*unstructured.Unstructured, 0)
	manifests := releaseutil.SplitManifests(service.YamlContent)
	for _, item := range manifests {
		u, err := serializer.NewDecoder().YamlToUnstructured([]byte(item))
		if err != nil {
			return "", errors.Errorf("failed to decode service %s yaml to unstructured: %v", service.ServiceName, err)
		}
		resources = append(resources, u)
	}
	deploymentNum := 0
	var yamls []string
	serviceName := service.ServiceName
	getNameWithNewTag := func(name, lastTag, newTag string) string {
		if !strings.HasSuffix(name, lastTag) {
			return name
		}
		return strings.TrimSuffix(name, lastTag) + newTag
	}
	for _, resource := range resources {
		switch resource.GetKind() {
		case setting.Deployment:
			if deploymentNum > 0 {
				return "", errors.Errorf("service-%s: only one deployment is allowed in each service", serviceName)
			}
			deploymentNum++

			deploymentObj := &v1.Deployment{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, deploymentObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s deployment to deployment object: %v", serviceName, err)
			}
			if deploymentObj.Spec.Selector == nil || !checkMapKeyExist(deploymentObj.Spec.Selector.MatchLabels, types.ZadigReleaseVersionLabelKey) {
				return "", errors.Errorf("service %s deployment label selector must contain %s", serviceName, types.ZadigReleaseVersionLabelKey)
			}
			if !checkMapKeyExist(deploymentObj.Spec.Template.Labels, types.ZadigReleaseVersionLabelKey) {
				return "", errors.Errorf("service %s deployment template label must contain %s", serviceName, types.ZadigReleaseVersionLabelKey)
			}

			deploymentObj.Name = getNameWithNewTag(deploymentObj.Name, lastGrayTag, grayTag)
			deploymentObj.Labels = setMseLabels(deploymentObj.Labels, grayTag, serviceName)
			deploymentObj.Spec.Selector.MatchLabels = setMseDeploymentLabels(deploymentObj.Spec.Selector.MatchLabels, grayTag, serviceName)
			deploymentObj.Spec.Template.Labels = setMseDeploymentLabels(deploymentObj.Spec.Template.Labels, grayTag, serviceName)
			Replicas := int32(service.Replicas)
			deploymentObj.Spec.Replicas = &Replicas
			resp, err := toYaml(deploymentObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s deployment object: %v", serviceName, err)
			}
			prod, err := commonrepo.NewProductColl().Find(&commonrepo.ProductFindOptions{
				Name:    productName,
				EnvName: envName,
			})
			if err != nil {
				return "", errors.Errorf("failed to find product %s: %v", productName, err)
			}
			serviceModules := sets.NewString()
			var newImages []*commonmodels.Container
			for _, image := range service.ServiceAndImage {
				serviceModules.Insert(image.ServiceModule)
				newImages = append(newImages, &commonmodels.Container{
					Name:  image.ServiceModule,
					Image: image.Image,
				})
			}
			for _, services := range prod.Services {
				for _, productService := range services {
					for _, container := range productService.Containers {
						if !serviceModules.Has(container.Name) {
							newImages = append(newImages, &commonmodels.Container{
								Name:  container.Name,
								Image: container.Image,
							})
						}
					}
				}
			}
			resp, _, err = kube.ReplaceWorkloadImages(resp, newImages)
			if err != nil {
				return "", errors.Errorf("failed to replace service %s deployment image: %v", serviceName, err)
			}
			yamls = append(yamls, resp)
		case setting.ConfigMap:
			cmObj := &corev1.ConfigMap{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, cmObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s ConfigMap to object: %v", serviceName, err)
			}
			cmObj.Name = getNameWithNewTag(cmObj.Name, lastGrayTag, grayTag)
			cmObj.SetLabels(setMseLabels(cmObj.GetLabels(), grayTag, serviceName))
			s, err := toYaml(cmObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s configmap object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		case setting.Service:
			serviceObj := &corev1.Service{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, serviceObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s Service to object: %v", serviceName, err)
			}
			serviceObj.Name = getNameWithNewTag(serviceObj.Name, lastGrayTag, grayTag)
			serviceObj.SetLabels(setMseLabels(serviceObj.GetLabels(), grayTag, serviceName))
			serviceObj.Spec.Selector = setMseLabels(serviceObj.Spec.Selector, grayTag, serviceName)
			s, err := toYaml(serviceObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s service object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		case setting.Secret:
			secretObj := &corev1.Secret{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, secretObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s Secret to object: %v", serviceName, err)
			}
			secretObj.Name = getNameWithNewTag(secretObj.Name, lastGrayTag, grayTag)
			secretObj.SetLabels(setMseLabels(secretObj.GetLabels(), grayTag, serviceName))
			s, err := toYaml(secretObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s secret object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		case setting.Ingress:
			ingressObj := &networkingv1.Ingress{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, ingressObj)
			if err != nil {
				return "", errors.Errorf("failed to convert service %s Ingress to object: %v", serviceName, err)
			}
			ingressObj.Name = getNameWithNewTag(ingressObj.Name, lastGrayTag, grayTag)
			ingressObj.SetLabels(setMseLabels(ingressObj.GetLabels(), grayTag, serviceName))
			s, err := toYaml(ingressObj)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s ingress object: %v", serviceName, err)
			}
			yamls = append(yamls, s)
		default:
			return "", errors.Errorf("service %s resource type %s not allowed", serviceName, resource.GetKind())
		}
	}
	if deploymentNum == 0 {
		return "", errors.Errorf("service %s must contain one deployment", serviceName)
	}
	return strings.Join(yamls, "---\n"), nil
}

func toYaml(obj runtime.Object) (string, error) {
	y := printers.YAMLPrinter{}
	writer := bytes.NewBuffer(nil)
	err := y.PrintObj(obj, writer)
	if err != nil {
		return "", errors.Wrapf(err, "failed to marshal object to yaml")
	}
	return writer.String(), nil
}

func GetMseOfflineResources(grayTag, envName, projectName string) ([]string, error) {
	prod, err := commonrepo.NewProductColl().Find(&commonrepo.ProductFindOptions{
		Name:    projectName,
		EnvName: envName,
	})
	if err != nil {
		return nil, errors.Errorf("failed to find product %s: %v", projectName, err)
	}
	kubeClient, err := kubeclient.GetKubeClient(config.HubServerAddress(), prod.ClusterID)
	if err != nil {
		return nil, err
	}
	selector := labels.Set{
		types.ZadigReleaseTypeLabelKey:    types.ZadigReleaseTypeMseGray,
		types.ZadigReleaseVersionLabelKey: grayTag,
	}.AsSelector()
	deploymentList, err := getter.ListDeployments(prod.Namespace, selector, kubeClient)
	if err != nil {
		return nil, errors.Errorf("can't list deployment: %v", err)
	}
	var services []string
	for _, deployment := range deploymentList {
		if service := deployment.Labels[types.ZadigReleaseServiceNameLabelKey]; service != "" {
			services = append(services, service)
		} else {
			log.Warnf("GetMseOfflineResources: deployment %s has no service name label", deployment.Name)
		}
	}

	return services, nil
}

func GetBlueGreenServiceK8sServiceYaml(projectName, envName, serviceName string) (string, error) {
	yamlContent, _, err := kube.FetchCurrentAppliedYaml(&kube.GeneSvcYamlOption{
		ProductName: projectName,
		EnvName:     envName,
		ServiceName: serviceName,
	})
	if err != nil {
		return "", errors.Errorf("failed to fetch %s current applied yaml, err: %s", serviceName, err)
	}
	resources := make([]*unstructured.Unstructured, 0)
	manifests := releaseutil.SplitManifests(yamlContent)
	for _, item := range manifests {
		u, err := serializer.NewDecoder().YamlToUnstructured([]byte(item))
		if err != nil {
			return "", errors.Errorf("failed to decode service %s yaml to unstructured: %v", serviceName, err)
		}
		resources = append(resources, u)
	}
	var (
		service     *corev1.Service
		serviceYaml string
	)
	for _, resource := range resources {
		switch resource.GetKind() {
		case setting.Service:
			if service != nil {
				return "", errors.Errorf("service %s has more than one service", serviceName)
			}
			service = &corev1.Service{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, service)
			if err != nil {
				log.Errorf("failed to convert service %s service to service object: %v\nyaml: %s", serviceName, err, yamlContent)
				return "", errors.Errorf("failed to convert service %s service to service object: %v", serviceName, err)
			}
			service.Name = service.Name + "-blue"
			if service.Spec.Selector == nil {
				service.Spec.Selector = make(map[string]string)
			}
			service.Spec.Selector[config.BlueGreenVerionLabelName] = config.BlueVersion
			serviceYaml, err = toYaml(service)
			if err != nil {
				return "", errors.Errorf("failed to marshal service %s service object: %v", serviceName, err)
			}
		}
	}
	return serviceYaml, nil
}

func GetMseTagsInEnv(envName, projectName string) ([]string, error) {
	prod, err := commonrepo.NewProductColl().Find(&commonrepo.ProductFindOptions{
		Name:    projectName,
		EnvName: envName,
	})
	if err != nil {
		return nil, errors.Errorf("failed to find product %s: %v", projectName, err)
	}
	kubeClient, err := kubeclient.GetKubeClient(config.HubServerAddress(), prod.ClusterID)
	if err != nil {
		return nil, err
	}
	selector := labels.Set{
		types.ZadigReleaseTypeLabelKey: types.ZadigReleaseTypeMseGray,
	}.AsSelector()
	deploymentList, err := getter.ListDeployments(prod.Namespace, selector, kubeClient)
	if err != nil {
		return nil, errors.Errorf("can't list deployment: %v", err)
	}
	tags := sets.NewString()
	for _, deployment := range deploymentList {
		if tag := deployment.Labels[types.ZadigReleaseVersionLabelKey]; tag != "" {
			tags.Insert(tag)
		} else {
			log.Warnf("GetMseTagsInEnv: deployment %s has no release version tag", deployment.Name)
		}
	}

	return tags.List(), nil
}

func setMseDeploymentLabels(labels map[string]string, grayTag, serviceName string) map[string]string {
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[types.ZadigReleaseVersionLabelKey] = grayTag
	labels[types.ZadigReleaseMSEGrayTagLabelKey] = grayTag
	labels[types.ZadigReleaseTypeLabelKey] = types.ZadigReleaseTypeMseGray
	labels[types.ZadigReleaseServiceNameLabelKey] = serviceName
	return labels
}

func setMseLabels(labels map[string]string, grayTag, serviceName string) map[string]string {
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[types.ZadigReleaseVersionLabelKey] = grayTag
	labels[types.ZadigReleaseTypeLabelKey] = types.ZadigReleaseTypeMseGray
	labels[types.ZadigReleaseServiceNameLabelKey] = serviceName
	return labels
}

func checkMapKeyExist(m map[string]string, key string) bool {
	if m == nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func filterServiceVars(serviceName string, deployContents []config.DeployContent, service *commonmodels.DeployService, serviceEnv *commonservice.EnvService) (*commonmodels.DeployService, error) {
	if serviceEnv == nil {
		return service, fmt.Errorf("service: %v do not exist", serviceName)
	}
	defaultUpdateConfig := false
	if slices.Contains(deployContents, config.DeployConfig) && serviceEnv.Updatable {
		defaultUpdateConfig = true
	}

	keySet := sets.NewString()
	if service == nil {
		service = &commonmodels.DeployService{}
	} else {
		for _, config := range service.VariableConfigs {
			keySet = keySet.Insert(config.VariableKey)
		}
	}

	service.VariableYaml = serviceEnv.VariableYaml
	service.ServiceName = serviceName
	service.Updatable = serviceEnv.Updatable
	service.UpdateConfig = defaultUpdateConfig

	service.VariableKVs = []*commontypes.RenderVariableKV{}
	service.LatestVariableKVs = []*commontypes.RenderVariableKV{}

	for _, svcVar := range serviceEnv.VariableKVs {
		if keySet.Has(svcVar.Key) && !svcVar.UseGlobalVariable {
			service.VariableKVs = append(service.VariableKVs, svcVar)
		}
	}
	for _, svcVar := range serviceEnv.LatestVariableKVs {
		if keySet.Has(svcVar.Key) && !svcVar.UseGlobalVariable {
			service.LatestVariableKVs = append(service.LatestVariableKVs, svcVar)
		}
	}
	if !slices.Contains(deployContents, config.DeployVars) {
		service.VariableKVs = []*commontypes.RenderVariableKV{}
		service.LatestVariableKVs = []*commontypes.RenderVariableKV{}
	}

	return service, nil
}

func getAllowedProjects(headers http.Header) ([]string, error) {
	type allowedProjectsData struct {
		Projects []string `json:"result"`
	}
	allowedProjects := &allowedProjectsData{}
	opaClient := opa.NewDefault()
	err := opaClient.Evaluate("rbac.user_allowed_projects", allowedProjects, func() (*opa.Input, error) {
		return generateOPAInput(headers, "GET", "/api/aslan/workflow/workflow"), nil
	})
	if err != nil {
		log.Errorf("get allowed projects error: %v", err)
		return nil, err
	}
	return allowedProjects.Projects, nil
}

func generateOPAInput(header http.Header, method string, endpoint string) *opa.Input {
	authorization := header.Get(strings.ToLower(setting.AuthorizationHeader))
	headers := map[string]string{}
	parsedPath := strings.Split(strings.Trim(endpoint, "/"), "/")
	headers[strings.ToLower(setting.AuthorizationHeader)] = authorization

	return &opa.Input{
		Attributes: &opa.Attributes{
			Request: &opa.Request{HTTP: &opa.HTTPSpec{
				Headers: headers,
				Method:  method,
			}},
		},
		ParsedPath: parsedPath,
	}
}
