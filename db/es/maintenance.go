package es

import (
	"fmt"

	"github.com/acronis/perfkit/db"
)

type indexPhase string

const (
	indexPhaseHot    indexPhase = "hot"
	indexPhaseWarm   indexPhase = "warm"
	indexPhaseCold   indexPhase = "cold"
	indexPhaseFrozen indexPhase = "frozen"
	indexPhaseDelete indexPhase = "delete"
)

type indexAction string

const (
	indexActionRollover           indexAction = "rollover"
	indexActionShrink             indexAction = "shrink"
	indexActionForceMerge         indexAction = "forcemerge"
	indexActionSearchableSnapshot indexAction = "searchable_snapshot"
	indexActionDelete             indexAction = "delete"
)

type indexRolloverActionSettings struct {
	MaxAge              string `json:"max_age,omitempty"`
	MaxDocs             string `json:"max_docs,omitempty"`
	MaxSize             string `json:"max_size,omitempty"`
	MaxPrimaryShardSize string `json:"max_primary_shard_size,omitempty"`
	MaxPrimaryShardDocs string `json:"max_primary_shard_docs,omitempty"`

	MinAge              string `json:"min_age,omitempty"`
	MinDocs             string `json:"min_docs,omitempty"`
	MinSize             string `json:"min_size,omitempty"`
	MinPrimaryShardSize string `json:"min_primary_shard_size,omitempty"`
	MinPrimaryShardDocs string `json:"min_primary_shard_docs,omitempty"`
}

type indexPhaseDefinition struct {
	MinAge  string                      `json:"min_age,omitempty"`
	Actions map[indexAction]interface{} `json:"actions,omitempty"`
}

type indexLifecycleManagementPolicy struct {
	Phases map[indexPhase]indexPhaseDefinition `json:"phases,omitempty"`
}

type indexSettings struct {
	NumberOfShards     int    `json:"number_of_shards,omitempty"`
	NumberOfReplicas   int    `json:"number_of_replicas,omitempty"`
	IndexLifeCycleName string `json:"index.lifecycle.name,omitempty"`
}

type fieldType string

const (
	fieldTypeLong        fieldType = "long"
	fieldTypeKeyword     fieldType = "keyword"
	fieldTypeText        fieldType = "text"
	fieldTypeBoolean     fieldType = "boolean"
	fieldTypeDateNano    fieldType = "date_nanos"
	fieldTypeDenseVector fieldType = "dense_vector"
)

type fieldSpec struct {
	Type    fieldType
	Dims    int
	Indexed bool
}

func convertToEsType(d dialect, t db.TableRow) fieldSpec {
	var spec = fieldSpec{
		Indexed: t.Indexed,
	}

	switch t.Type {
	case db.DataTypeBigIntAutoInc, db.DataTypeId, db.DataTypeInt:
		spec.Type = fieldTypeLong
	case db.DataTypeUUID:
		spec.Type = fieldTypeKeyword
	case db.DataTypeVarChar:
		spec.Type = fieldTypeKeyword
	case db.DataTypeText:
		spec.Type = fieldTypeText
	case db.DataTypeDateTime:
		spec.Type = fieldTypeDateNano
	case db.DataTypeBoolean:
		spec.Type = fieldTypeBoolean
	case db.DataTypeVector3Float32:
		spec.Type = d.getVectorType()
		spec.Dims = 3
	case db.DataTypeVector768Float32:
		spec.Type = d.getVectorType()
		spec.Dims = 768
	default:
		spec.Type = fieldTypeKeyword
	}

	return spec
}

func (s fieldSpec) MarshalJSON() ([]byte, error) {
	if s.Dims > 0 {
		if s.Indexed {
			return []byte(fmt.Sprintf(`{"type":%q, "dims":%d}`, s.Type, s.Dims)), nil
		}

		return []byte(fmt.Sprintf(`{"type":%q, "dims":%d, "index": false}`, s.Type, s.Dims)), nil
	}

	if s.Indexed {
		return []byte(fmt.Sprintf(`{"type":%q}`, s.Type)), nil
	}
	return []byte(fmt.Sprintf(`{"type":%q, "index": false}`, s.Type)), nil
}

type mapping map[string]fieldSpec
type indexName string

type mappings struct {
	Properties mapping `json:"properties"`
}

type componentTemplate struct {
	Settings *indexSettings `json:"settings,omitempty"`
	Mappings *mappings      `json:"mappings,omitempty"`
}

type migrator interface {
	checkILMPolicyExists(policyName string) (bool, error)
	initILMPolicy(policyName string, policyDefinition indexLifecycleManagementPolicy) error
	deleteILMPolicy(policyName string) error

	initComponentTemplate(templateName string, template componentTemplate) error
	deleteComponentTemplate(templateName string) error

	initIndexTemplate(templateName string, indexPattern string, components []string) error
	deleteIndexTemplate(templateName string) error

	deleteDataStream(dataStreamName string) error
}

func indexExists(mig migrator, tableName string) (bool, error) {
	var ilmPolicyName = fmt.Sprintf("ilm-data-5gb-%s", tableName)
	return mig.checkILMPolicyExists(ilmPolicyName)
}

func createIndex(d dialect, mig migrator, indexName string, indexDefinition *db.TableDefinition, tableMigrationDDL string) error {
	if err := createSearchQueryBuilder(indexName, indexDefinition.TableRows); err != nil {
		return err
	}

	var ilmPolicyName = fmt.Sprintf("ilm-data-5gb-%s", indexName)
	var ilmPolicy = indexLifecycleManagementPolicy{
		Phases: map[indexPhase]indexPhaseDefinition{
			indexPhaseHot: {
				Actions: map[indexAction]interface{}{
					indexActionRollover: indexRolloverActionSettings{
						MaxPrimaryShardSize: "5gb",
					},
				},
			},
			indexPhaseDelete: {
				MinAge: "90d",
				Actions: map[indexAction]interface{}{
					indexActionDelete: struct{}{},
				},
			},
		},
	}
	var ilmSettingName = fmt.Sprintf("ilm-settings-%s", indexName)

	if exists, err := mig.checkILMPolicyExists(ilmPolicyName); err != nil {
		return err
	} else if exists {
		return nil
	}

	if err := mig.initILMPolicy(ilmPolicyName, ilmPolicy); err != nil {
		return err
	}

	if err := mig.initComponentTemplate(ilmSettingName, componentTemplate{
		Settings: &indexSettings{
			IndexLifeCycleName: ilmPolicyName,
		},
	}); err != nil {
		return err
	}

	var numberOfShards = indexDefinition.Resilience.NumberOfShards
	if numberOfShards == 0 {
		numberOfShards = 1
	}

	var indexResilienceSettings = indexSettings{
		NumberOfShards:   numberOfShards,
		NumberOfReplicas: indexDefinition.Resilience.NumberOfReplicas,
	}

	var mappingTemplateName = fmt.Sprintf("mapping-%s", indexName)
	if len(indexDefinition.TableRows) == 0 {
		return fmt.Errorf("empty mapping")
	}

	if err := createSearchQueryBuilder(indexName, indexDefinition.TableRows); err != nil {
		return err
	}

	var mp = make(mapping)
	for _, row := range indexDefinition.TableRows {
		if row.Name == "id" {
			continue
		}

		mp[row.Name] = convertToEsType(d, row)
	}

	if err := mig.initComponentTemplate(mappingTemplateName, componentTemplate{
		Settings: &indexResilienceSettings,
		Mappings: &mappings{Properties: mp},
	}); err != nil {
		return err
	}

	var indexPattern = fmt.Sprintf("%s*", indexName)
	if err := mig.initIndexTemplate(indexName, indexPattern, []string{ilmSettingName, mappingTemplateName}); err != nil {
		return err
	}

	return nil
}

func dropIndex(mig migrator, indexName string) error {
	var dataStreamName = indexName
	if err := mig.deleteDataStream(dataStreamName); err != nil {
		return err
	}

	if err := mig.deleteIndexTemplate(dataStreamName); err != nil {
		return err
	}

	var ilmSettingName = fmt.Sprintf("ilm-settings-%s", indexName)
	if err := mig.deleteComponentTemplate(ilmSettingName); err != nil {
		return err
	}

	var ilmPolicyName = fmt.Sprintf("ilm-data-5gb-%s", indexName)
	if err := mig.deleteILMPolicy(ilmPolicyName); err != nil {
		return err
	}

	return nil
}
