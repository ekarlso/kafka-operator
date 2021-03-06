// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cruisecontrol

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/banzaicloud/kafka-operator/api/v1beta1"
	"github.com/banzaicloud/kafka-operator/pkg/resources/templates"
	"github.com/banzaicloud/kafka-operator/pkg/util"
	kafkautils "github.com/banzaicloud/kafka-operator/pkg/util/kafka"
	zookeeperutils "github.com/banzaicloud/kafka-operator/pkg/util/zookeeper"

	corev1 "k8s.io/api/core/v1"
)

func (r *Reconciler) configMap(log logr.Logger, clientPass string) runtime.Object {
	configMap := &corev1.ConfigMap{
		ObjectMeta: templates.ObjectMeta(
			fmt.Sprintf(configAndVolumeNameTemplate, r.KafkaCluster.Name),
			util.MergeLabels(ccLabelSelector(r.KafkaCluster.Name), r.KafkaCluster.Labels),
			r.KafkaCluster,
		),
		Data: map[string]string{
			"cruisecontrol.properties": r.KafkaCluster.Spec.CruiseControlConfig.Config + fmt.Sprintf(`
    # The Kafka cluster to control.
    bootstrap.servers=%s:%d
    # The zookeeper connect of the Kafka cluster
    zookeeper.connect=%s
`, generateBootstrapServer(r.KafkaCluster.Spec.HeadlessServiceEnabled, r.KafkaCluster.Name), r.KafkaCluster.Spec.ListenersConfig.InternalListeners[0].ContainerPort, zookeeperutils.PrepareConnectionAddress(r.KafkaCluster.Spec.ZKAddresses, r.KafkaCluster.Spec.GetZkPath())) +
				generateSSLConfig(&r.KafkaCluster.Spec.ListenersConfig, clientPass),
			"capacity.json":       GenerateCapacityConfig(r.KafkaCluster, log),
			"clusterConfigs.json": r.KafkaCluster.Spec.CruiseControlConfig.ClusterConfig,
			"log4j.properties": `
log4j.rootLogger = INFO, FILE
    log4j.appender.FILE=org.apache.log4j.FileAppender
    log4j.appender.FILE.File=/dev/stdout
    log4j.appender.FILE.layout=org.apache.log4j.PatternLayout
    log4j.appender.FILE.layout.conversionPattern=%-6r [%15.15t] %-5p %30.30c %x - %m%n
`,
			"log4j2.xml": `
<?xml version="1.0" encoding="UTF-8"?>
    <Configuration status="INFO">
        <Appenders>
            <File name="Console" fileName="/dev/stdout">
                <PatternLayout pattern="%d{yyy-MM-dd HH:mm:ss.SSS} [%t] %-5level %logger{36} - %msg%n"/>
            </File>
        </Appenders>
        <Loggers>
            <Root level="info">
                <AppenderRef ref="Console" />
            </Root>
        </Loggers>
    </Configuration>
`,
		},
	}
	return configMap
}

func generateSSLConfig(l *v1beta1.ListenersConfig, clientPass string) (res string) {
	if l.SSLSecrets != nil && util.IsSSLEnabledForInternalCommunication(l.InternalListeners) {
		res = fmt.Sprintf(`
security.protocol=SSL
ssl.truststore.location=/var/run/secrets/java.io/keystores/tls.jks
ssl.keystore.location=/var/run/secrets/java.io/keystores/tls.jks
ssl.keystore.password=%s
ssl.truststore.password=%s
`, clientPass, clientPass)
	}
	return
}

func generateBootstrapServer(headlessEnabled bool, clusterName string) string {
	if headlessEnabled {
		return fmt.Sprintf(kafkautils.HeadlessServiceTemplate, clusterName)
	}
	return fmt.Sprintf(kafkautils.AllBrokerServiceTemplate, clusterName)
}

const (
	storageConfigCPUDefaultValue   = "100"
	storageConfigNWINDefaultValue  = "10000"
	storageConfigNWOUTDefaultValue = "10000"
)

type CruiseControlCapacityConfig struct {
	BrokerCapacities []BrokerCapacity `json:"brokerCapacities"`
}
type BrokerCapacity struct {
	BrokerID string   `json:"brokerId"`
	Capacity Capacity `json:"capacity"`
	Doc      string   `json:"doc"`
}
type Capacity struct {
	DISK  map[string]string `json:"DISK"`
	CPU   string            `json:"CPU"`
	NWIN  string            `json:"NW_IN"`
	NWOUT string            `json:"NW_OUT"`
}

// generateCapacityConfig generates a CC capacity config with default values or returns the manually overridden value if it exists
func GenerateCapacityConfig(kafkaCluster *v1beta1.KafkaCluster, log logr.Logger) string {
	log.Info("Generating capacity config")

	// If there is already a config added manually, use that one
	if kafkaCluster.Spec.CruiseControlConfig.CapacityConfig != "" {
		return kafkaCluster.Spec.CruiseControlConfig.CapacityConfig
	}

	capacityConfig := CruiseControlCapacityConfig{}

	for _, brokerState := range kafkaCluster.Spec.Brokers {
		brokerCapacity := BrokerCapacity{
			BrokerID: fmt.Sprintf("%d", brokerState.Id),
			Capacity: Capacity{
				DISK:  generateBrokerDisks(brokerState, kafkaCluster.Spec, log),
				CPU:   storageConfigCPUDefaultValue,
				NWIN:  storageConfigNWINDefaultValue,
				NWOUT: storageConfigNWOUTDefaultValue,
			},
			Doc: "Capacity unit used for disk is in MB, cpu is in percentage, network throughput is in KB.",
		}

		log.Info("The following brokerCapacity was generated", "brokerCapacity", brokerCapacity)

		capacityConfig.BrokerCapacities = append(capacityConfig.BrokerCapacities, brokerCapacity)
	}

	result, err := json.MarshalIndent(capacityConfig, "", "    ")
	if err != nil {
		log.Error(err, "Could not marshal cruise control capacity config")
	}
	log.Info("Generated capacity config was successful with values:", result)

	return string(result)
}

func generateBrokerDisks(brokerState v1beta1.Broker, kafkaClusterSpec v1beta1.KafkaClusterSpec, log logr.Logger) map[string]string {
	brokerDisks := map[string]string{}

	// Get disks from the BrokerConfigGroup if it's in use
	if brokerState.BrokerConfigGroup != "" {
		brokerConfigGroup := kafkaClusterSpec.BrokerConfigGroups[brokerState.BrokerConfigGroup]
		parseMountPathWithSize(brokerConfigGroup, log, brokerState, brokerDisks)
	}

	//Get disks from the BrokerConfig itself
	if brokerState.BrokerConfig != nil {
		parseMountPathWithSize(*brokerState.BrokerConfig, log, brokerState, brokerDisks)
	}

	return brokerDisks
}

func parseMountPathWithSize(brokerConfigGroup v1beta1.BrokerConfig, log logr.Logger, brokerState v1beta1.Broker, brokerDisks map[string]string) {
	for _, storageConfig := range brokerConfigGroup.StorageConfigs {
		int64Value, isConvertible := util.QuantityPointer(storageConfig.PvcSpec.Resources.Requests["storage"]).AsInt64()
		if isConvertible == false {
			log.Info("Could not convert 'storage' quantity to Int64 in brokerConfig for broker",
				"brokerId", brokerState.Id)
		}

		brokerDisks[storageConfig.MountPath+"/kafka"] = strconv.FormatInt(int64Value, 10)
	}
}
