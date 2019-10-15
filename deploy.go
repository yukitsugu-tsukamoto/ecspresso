package ecspresso

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/codedeploy"
	"github.com/aws/aws-sdk-go/service/ecs"
	isatty "github.com/mattn/go-isatty"
	"github.com/pkg/errors"
)

const (
	CodeDeployConsoleURLFmt = "https://%s.console.aws.amazon.com/codesuite/codedeploy/deployments/%s?region=%s"
	AppSpecFmtWithLB        = `version: 1
Resources:
- TargetService:
    Type: AWS::ECS::Service
    Properties:
      TaskDefinition: "%s"
      LoadBalancerInfo:
        ContainerName: %s
        ContainerPort: %d
`
	AppSpecFmtWithoutLB = `version: 1
Resources:
- TargetService:
    Type: AWS::ECS::Service
    Properties:
      TaskDefinition: "%s"
`
)

func (d *App) Deploy(opt DeployOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Starting deploy")
	sv, err := d.DescribeServiceStatus(ctx, 0)
	if err != nil {
		return errors.Wrap(err, "failed to describe service status")
	}

	var count *int64
	if sv.SchedulingStrategy != nil && *sv.SchedulingStrategy == "DAEMON" {
		count = nil
	} else if *opt.DesiredCount == KeepDesiredCount {
		count = sv.DesiredCount
	} else {
		count = opt.DesiredCount
	}

	var tdArn string
	if *opt.SkipTaskDefinition {
		tdArn = *sv.TaskDefinition
	} else {
		td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
		if err != nil {
			return errors.Wrap(err, "failed to load task definition")
		}
		if *opt.DryRun {
			d.Log("task definition:", td.String())
		} else {
			newTd, err := d.RegisterTaskDefinition(ctx, td)
			if err != nil {
				return errors.Wrap(err, "failed to register task definition")
			}
			tdArn = *newTd.TaskDefinitionArn
		}
	}
	if count != nil {
		d.Log("desired count:", *count)
	}
	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	// manage auto scaling only when set option --suspend-auto-scaling or --no-suspend-auto-scaling explicitly
	if suspend := opt.SuspendAutoScaling; suspend != nil {
		if err := d.suspendAutoScaling(*suspend); err != nil {
			return err
		}
	}

	// detect controller
	if dc := sv.DeploymentController; dc != nil {
		switch t := *dc.Type; t {
		case "CODE_DEPLOY":
			return d.DeployByCodeDeploy(ctx, tdArn, count, sv, opt)
		default:
			return fmt.Errorf("could not deploy a service using deployment controller type %s", t)
		}
	}

	// rolling deploy (ECS internal)
	if err := d.UpdateService(ctx, tdArn, count, *opt.ForceNewDeployment, sv); err != nil {
		return errors.Wrap(err, "failed to update service")
	}

	if *opt.NoWait {
		d.Log("Service is deployed.")
		return nil
	}

	time.Sleep(delayForServiceChanged) // wait for service updated
	if err := d.WaitServiceStable(ctx, time.Now()); err != nil {
		return errors.Wrap(err, "failed to wait service stable")
	}

	d.Log("Service is stable now. Completed!")
	return nil
}

func (d *App) UpdateService(ctx context.Context, taskDefinitionArn string, count *int64, force bool, sv *ecs.Service) error {
	msg := "Updating service"
	if force {
		msg = msg + " with force new deployment"
	}
	msg = msg + "..."
	d.Log(msg)

	_, err := d.ecs.UpdateServiceWithContext(
		ctx,
		&ecs.UpdateServiceInput{
			Service:                       aws.String(d.Service),
			Cluster:                       aws.String(d.Cluster),
			TaskDefinition:                aws.String(taskDefinitionArn),
			DesiredCount:                  count,
			ForceNewDeployment:            &force,
			NetworkConfiguration:          sv.NetworkConfiguration,
			HealthCheckGracePeriodSeconds: sv.HealthCheckGracePeriodSeconds,
			PlatformVersion:               sv.PlatformVersion,
		},
	)
	return err
}

func (d *App) DeployByCodeDeploy(ctx context.Context, taskDefinitionArn string, count *int64, sv *ecs.Service, opt DeployOption) error {
	if *sv.DesiredCount != *count {
		d.Log("updating desired count to", *count)
		_, err := d.ecs.UpdateServiceWithContext(
			ctx,
			&ecs.UpdateServiceInput{
				Service:      aws.String(d.Service),
				Cluster:      aws.String(d.Cluster),
				DesiredCount: count,
			},
		)
		if err != nil {
			return errors.Wrap(err, "failed to update service")
		}
	}

	var appSpec string
	if sv.LoadBalancers != nil && len(sv.LoadBalancers) > 0 {
		appSpec = fmt.Sprintf(
			AppSpecFmtWithLB,
			taskDefinitionArn,
			*sv.LoadBalancers[0].ContainerName,
			*sv.LoadBalancers[0].ContainerPort,
		)
	} else {
		appSpec = fmt.Sprintf(AppSpecFmtWithoutLB, taskDefinitionArn)
	}
	d.DebugLog("appSpecContent:", appSpec)

	// deployment
	dp, err := d.findDeployment(sv)
	if err != nil {
		return err
	}
	dd := &codedeploy.CreateDeploymentInput{
		ApplicationName:      dp.ApplicationName,
		DeploymentGroupName:  dp.DeploymentGroupName,
		DeploymentConfigName: dp.DeploymentConfigName,
		Revision: &codedeploy.RevisionLocation{
			RevisionType: aws.String("AppSpecContent"),
			AppSpecContent: &codedeploy.AppSpecContent{
				Content: aws.String(appSpec),
			},
		},
	}
	if ev := *opt.RollbackEvents; ev != "" {
		var events []*string
		for _, ev := range strings.Split(ev, ",") {
			events = append(events, aws.String(ev))
		}
		dd.AutoRollbackConfiguration = &codedeploy.AutoRollbackConfiguration{
			Enabled: aws.Bool(true),
			Events:  events,
		}
	}
	d.DebugLog("creating a deployment to CodeDeploy", dd.String())

	res, err := d.codedeploy.CreateDeploymentWithContext(ctx, dd)
	if err != nil {
		return errors.Wrap(err, "failed to create deployment")
	}
	id := *res.DeploymentId
	u := fmt.Sprintf(
		CodeDeployConsoleURLFmt,
		d.config.Region,
		id,
		d.config.Region,
	)
	d.Log(fmt.Sprintf("Deployment %s is created on CodeDeploy:", id))
	d.Log(u)

	if isatty.IsTerminal(os.Stdout.Fd()) {
		if err := exec.Command("open", u).Start(); err != nil {
			d.Log("Couldn't open URL", u)
		}
	}
	return nil
}

func (d *App) findDeployment(sv *ecs.Service) (*codedeploy.DeploymentInfo, error) {
	if len(sv.TaskSets) == 0 {
		return nil, errors.New("taskSet is not found in service")
	}
	dp, err := d.codedeploy.GetDeployment(&codedeploy.GetDeploymentInput{
		DeploymentId: sv.TaskSets[0].ExternalId,
	})
	if err != nil {
		return nil, err
	}
	return dp.DeploymentInfo, nil
}
