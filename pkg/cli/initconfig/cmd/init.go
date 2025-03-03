/*
Copyright 2021 The KodeRover Authors.

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

package cmd

import (
	_ "embed"
	"time"

	"github.com/spf13/cobra"

	"github.com/koderover/zadig/pkg/config"
	"github.com/koderover/zadig/pkg/setting"
	"github.com/koderover/zadig/pkg/shared/client/aslan"
	kubeclient "github.com/koderover/zadig/pkg/shared/kube/client"
	"github.com/koderover/zadig/pkg/tool/kube/updater"
	"github.com/koderover/zadig/pkg/tool/log"
)

func init() {
	rootCmd.AddCommand(initCmd)
	log.Init(&log.Config{
		Level: config.LogLevel(),
	})
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "init system config",
	Long:  `init system config.`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := run(); err != nil {
			log.Fatal(err)
		}
	},
}

func run() error {
	for {
		err := Healthz()
		if err == nil {
			break
		}
		log.Error(err)
		time.Sleep(10 * time.Second)
	}
	err := initSystemConfig()
	if err == nil {
		log.Info("zadig init success")
	}
	return err
}

func initSystemConfig() error {
	if err := createLocalCluster(); err != nil {
		log.Errorf("createLocalCluster err:%s", err)
		return err
	}

	if err := scaleWarpdrive(); err != nil {
		log.Errorf("scale warpdrive err: %s", err)
		return err
	}

	if config.Enterprise() {
		// only initialize user for enterprise version
		if err := initializeAdminUser(); err != nil {
			log.Errorf("initialize admin user failed: email: %s, password: %s, err: %s", config.AdminEmail(), config.AdminPassword(), err)
			return err
		}
	}

	return nil
}

func scaleWarpdrive() error {
	cfg, err := aslan.New(config.AslanServiceAddress()).GetWorkflowConcurrencySetting()
	if err == nil {
		client, err := kubeclient.GetKubeClient(config.HubServerServiceAddress(), setting.LocalClusterID)
		if err != nil {
			return err
		}
		return updater.ScaleDeployment(config.Namespace(), config.WarpDriveServiceName(), int(cfg.WorkflowConcurrency), client)
	}

	log.Errorf("Failed to get workflow concurrency settings, error: %s", err)
	return err
}

func createLocalCluster() error {
	cluster, err := aslan.New(config.AslanServiceAddress()).GetLocalCluster()
	if err != nil {
		return err
	}
	if cluster != nil {
		return nil
	}
	return aslan.New(config.AslanServiceAddress()).AddLocalCluster()
}

func initializeAdminUser() error {
	username := "admin"
	password := config.AdminPassword()
	email := config.AdminEmail()

	return aslan.New(config.AslanServiceAddress()).InitializeUser(username, password, email)
}
