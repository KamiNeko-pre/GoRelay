package information

import (
	"sync"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm/schema"
)

func TestInformationStepNamespacesFitTheirSQLKeyColumn(t *testing.T) {
	model, err := schema.Parse(&Step{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	column := model.LookUpField("Key")
	for _, kind := range []string{"trial_api", "inspect_api_candidate", "lua_failure_report", "post_acquisition_rag"} {
		key := kind + ":" + agentdata.Hash(map[string]any{"url": "https://source.example/items"})
		if len(key) > column.Size {
			t.Errorf("%s produces %d bytes, SQL column accepts %d", kind, len(key), column.Size)
		}
	}
}
