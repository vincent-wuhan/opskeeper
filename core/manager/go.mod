// Module manager is the control plane: the API, the identity system, the
// approval and audit ledgers, the topology graph, the knowledge base, and
// the adapters that reach into managed systems.
//
// This module is the last piece of the 2.0 split. It exists so that a node
// package importing the control plane is a compile error rather than a
// review comment, and so that the control plane can be versioned and
// released on its own line.
//
// Invariants:
//
//   - manager never imports github.com/MichaelKinsy/PiG directly. Model
//     access goes through core/pig, which is the single place a PiG API
//     change has to be absorbed.
//   - manager depends on core (contracts) and floor (the infrastructure
//     both planes share). It does not *link* core/edge: the node plane is
//     told what to do over the tunnel, it is never linked in. Three test
//     files under biz/nodefleet/e2e and biz/aiops/tools import the node
//     plane's policy gate and command policy, which is why core/edge is a
//     require at all — Go has no test-only require. scripts/modulecheck's
//     testOnlyImports table is what keeps that to tests: a non-test file
//     here importing core/edge is a boundary violation, not a code review
//     comment.
//
// Subpackages: biz/data/model/server/service are the control plane proper;
// iam is the identity system; pkg, middleware, control, knowledge,
// dataguard, agentteams, observability, higress, migrate and migrator are
// the infrastructure those layers run on. iam and the control plane stay two
// bounded contexts inside one module — the module system cannot tell them
// apart, so scripts/modulecheck's BC rules do.
module github.com/vincent-wuhan/opskeeper/core/manager

go 1.26.0

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/anush008/fastembed-go v1.0.0
	github.com/casbin/casbin/v2 v2.103.0
	github.com/casbin/gorm-adapter/v3 v3.32.0
	github.com/glebarez/go-sqlite v1.22.0
	github.com/glebarez/sqlite v1.11.0
	github.com/go-chi/chi/v5 v5.1.0
	github.com/golang-jwt/jwt/v5 v5.3.0
	github.com/golang/snappy v0.0.4
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgx/v5 v5.8.0
	github.com/larksuite/oapi-sdk-go/v3 v3.5.4
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728
	github.com/prometheus/client_golang v1.20.5
	github.com/prometheus/client_model v0.6.1
	github.com/redis/go-redis/v9 v9.21.0
	github.com/robfig/cron/v3 v3.0.1
	github.com/segmentio/kafka-go v0.4.51
	github.com/singchia/frontier v1.2.3-rc.2
	github.com/singchia/geminio v1.2.3-rc.1
	github.com/stretchr/testify v1.11.1
	github.com/vincent-wuhan/opskeeper/core v0.0.0
	github.com/vincent-wuhan/opskeeper/core/base v0.0.0
	github.com/vincent-wuhan/opskeeper/core/domains v0.0.0
	github.com/vincent-wuhan/opskeeper/core/edge v0.0.0
	github.com/vincent-wuhan/opskeeper/core/extension v0.0.0
	github.com/vincent-wuhan/opskeeper/core/floor v0.0.0
	github.com/vincent-wuhan/opskeeper/core/pig v0.0.0-00010101000000-000000000000
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/trace v1.43.0
	golang.org/x/crypto v0.55.0
	golang.org/x/sync v0.22.0
	golang.org/x/time v0.15.0
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
	gorm.io/driver/mysql v1.6.0
	gorm.io/driver/postgres v1.6.0
	gorm.io/gorm v1.31.1
	gorm.io/plugin/soft_delete v1.2.1
)

require (
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/Masterminds/semver/v3 v3.5.0 // indirect
	github.com/MichaelKinsy/PiG v0.4.0 // indirect
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/aws/aws-sdk-go-v2 v1.41.7 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.10 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.17 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.16 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.50.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.42.1 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/casbin/govaluate v1.10.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/x/ansi v0.11.7 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/deckarep/golang-set/v2 v2.6.0 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-kratos/aegis v0.2.0 // indirect
	github.com/go-kratos/kratos/v2 v2.7.2 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-playground/form/v4 v4.2.0 // indirect
	github.com/go-sql-driver/mysql v1.9.3 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/jumboframes/armorigo v0.4.1 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.23 // indirect
	github.com/microsoft/go-mssqldb v1.9.5 // indirect
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/schollz/progressbar/v2 v2.15.0 // indirect
	github.com/schollz/progressbar/v3 v3.14.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/singchia/go-timer/v2 v2.2.1 // indirect
	github.com/singchia/yafsm v1.0.1 // indirect
	github.com/sugarme/regexpset v0.0.0-20200920021344-4d4ec8eaf93c // indirect
	github.com/sugarme/tokenizer v0.2.3-0.20230829214935-448e79b1ed65 // indirect
	github.com/vincent-wuhan/opskeeper/sdk v0.0.0 // indirect
	github.com/yalue/onnxruntime_go v1.7.0 // indirect
	github.com/yuin/goldmark v1.8.5 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/image v0.45.0 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	gorm.io/driver/sqlserver v1.6.3 // indirect
	gorm.io/plugin/dbresolver v1.6.2 // indirect
	k8s.io/klog/v2 v2.120.1 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.53.0 // indirect
)

replace (
	github.com/vincent-wuhan/opskeeper/core/base => ../base
	github.com/vincent-wuhan/opskeeper/core/domains => ../domains
	github.com/vincent-wuhan/opskeeper/core => ../
	github.com/vincent-wuhan/opskeeper/core/edge => ../edge
	github.com/vincent-wuhan/opskeeper/core/extension => ../extension
	github.com/vincent-wuhan/opskeeper/core/floor => ../floor
	github.com/vincent-wuhan/opskeeper/core/pig => ../pig
	github.com/vincent-wuhan/opskeeper/sdk => ../../sdk
)
