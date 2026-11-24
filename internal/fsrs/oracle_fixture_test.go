package fsrs

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"testing"
)

//go:embed testdata/py-fsrs-6.3.1-vectors.json
var pythonFixture []byte

type fixtureMetadata struct {
	SchemaVersion int `json:"schemaVersion"`
	Oracle        struct {
		Package           string `json:"package"`
		PackageVersion    string `json:"packageVersion"`
		AlgorithmVersion  string `json:"algorithmVersion"`
		ReleaseCommit     string `json:"releaseCommit"`
		License           string `json:"license"`
		WheelSHA256       string `json:"wheelSha256"`
		ContainerDigest   string `json:"containerDigest"`
		ContainerPlatform string `json:"containerPlatform"`
	} `json:"oracle"`
	CaseCount      int `json:"caseCount"`
	Configurations []struct {
		ID            string    `json:"id"`
		Parameters    []float64 `json:"parameters"`
		EnableFuzzing bool      `json:"enableFuzzing"`
	} `json:"configurations"`
	Cases []struct {
		Name string `json:"name"`
	} `json:"cases"`
}

func TestPythonOracleFixtureIsPinned(t *testing.T) {
	t.Parallel()

	const expectedSHA256 = "27c0777605a7ce95e1eaf38200bbe2e0b6daf41d8227486a6b4a84d427db8c1c"
	digest := sha256.Sum256(pythonFixture)
	if got := hex.EncodeToString(digest[:]); got != expectedSHA256 {
		t.Fatalf("fixture SHA-256 = %s, want %s", got, expectedSHA256)
	}

	var fixture fixtureMetadata
	if err := json.Unmarshal(pythonFixture, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	if fixture.SchemaVersion != 1 {
		t.Fatalf("schema version = %d, want 1", fixture.SchemaVersion)
	}
	if fixture.Oracle.Package != "fsrs" || fixture.Oracle.PackageVersion != "6.3.1" {
		t.Fatalf("oracle = %s@%s, want fsrs@6.3.1", fixture.Oracle.Package, fixture.Oracle.PackageVersion)
	}
	if fixture.Oracle.AlgorithmVersion != "FSRS-6" {
		t.Fatalf("algorithm = %q, want FSRS-6", fixture.Oracle.AlgorithmVersion)
	}
	if fixture.Oracle.ReleaseCommit != "3abe686e9c058d3f3c00bbeb92e68b71211b2b31" {
		t.Fatalf("unexpected oracle commit %q", fixture.Oracle.ReleaseCommit)
	}
	if fixture.Oracle.License != "MIT" {
		t.Fatalf("oracle license = %q, want MIT", fixture.Oracle.License)
	}
	if fixture.Oracle.WheelSHA256 != "ac1bf9939573592d8c9bc1e11a00bd17e04146dc9f2c913127e2bcc431b9040b" {
		t.Fatalf("unexpected wheel SHA-256 %q", fixture.Oracle.WheelSHA256)
	}
	if fixture.Oracle.ContainerDigest != "sha256:f1927c75e81efd1e091dbd64b6c0ecaa5630b38635a3d1c04034ac636e1f94c8" {
		t.Fatalf("unexpected container digest %q", fixture.Oracle.ContainerDigest)
	}
	if fixture.Oracle.ContainerPlatform != "linux/amd64" {
		t.Fatalf("unexpected container platform %q", fixture.Oracle.ContainerPlatform)
	}
	if fixture.CaseCount != 705 || len(fixture.Cases) != fixture.CaseCount {
		t.Fatalf("fixture has caseCount=%d and %d cases, want 705", fixture.CaseCount, len(fixture.Cases))
	}
	if len(fixture.Configurations) != 11 {
		t.Fatalf("fixture has %d configurations, want 11", len(fixture.Configurations))
	}
	for _, configuration := range fixture.Configurations {
		if len(configuration.Parameters) != 21 {
			t.Fatalf("configuration %q has %d parameters, want 21", configuration.ID, len(configuration.Parameters))
		}
		if configuration.EnableFuzzing {
			t.Fatalf("configuration %q unexpectedly enables fuzzing", configuration.ID)
		}
	}
}
