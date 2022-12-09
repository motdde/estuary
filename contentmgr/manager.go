package contentmgr

import (
	"sync"

	"github.com/application-research/estuary/config"
	"github.com/application-research/estuary/drpc"
	"github.com/application-research/estuary/miner"
	"github.com/application-research/estuary/node"
	"github.com/application-research/estuary/shuttle"
	"github.com/application-research/estuary/transfer"
	"github.com/application-research/estuary/util"
	"github.com/application-research/filclient"
	"github.com/filecoin-project/lotus/api"
	lru "github.com/hashicorp/golang-lru"
	"github.com/ipfs/go-cid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type ContentManager struct {
	db                   *gorm.DB
	api                  api.Gateway
	filClient            *filclient.FilClient
	node                 *node.Node
	cfg                  *config.Estuary
	tracer               trace.Tracer
	blockstore           node.EstuaryBlockstore
	notifyBlockstore     *node.NotifyBlockstore
	toCheck              chan uint
	queueMgr             *queueManager
	retrLk               sync.Mutex
	retrievalsInProgress map[uint]*util.RetrievalProgress
	contentLk            sync.RWMutex

	dealDisabledLk       sync.Mutex
	isDealMakingDisabled bool

	shuttleMgr           *shuttle.Manager
	remoteTransferStatus *lru.ARCCache
	inflightCids         map[cid.Cid]uint
	inflightCidsLk       sync.Mutex
	IncomingRPCMessages  chan *drpc.Message
	minerManager         miner.IMinerManager
	log                  *zap.SugaredLogger
	transferMgr          *transfer.Manager

	// consolidatingZonesLk is used to serialize reads and writes to consolidatingZones
	consolidatingZonesLk sync.Mutex
	// aggregatingZonesLk is used to serialize reads and writes to aggregatingZones
	aggregatingZonesLk sync.Mutex
	// addStagingContentLk is used to serialize content adds to staging zones
	// otherwise, we'd risk creating multiple "initial" staging zones, or exceeding MaxDealContentSize
	addStagingContentLk sync.Mutex

	consolidatingZones map[uint]bool
	aggregatingZones   map[uint]bool
}

func NewContentManager(
	db *gorm.DB,
	api api.Gateway,
	fc *filclient.FilClient,
	tbs *util.TrackingBlockstore,
	nd *node.Node,
	cfg *config.Estuary,
	minerManager miner.IMinerManager,
	log *zap.SugaredLogger,
	shuttleMgr *shuttle.Manager,
	transferMgr *transfer.Manager,
) (*ContentManager, error) {
	cache, err := lru.NewARC(50000)
	if err != nil {
		return nil, err
	}

	cm := &ContentManager{
		cfg:                  cfg,
		db:                   db,
		api:                  api,
		filClient:            fc,
		blockstore:           tbs.Under().(node.EstuaryBlockstore),
		node:                 nd,
		notifyBlockstore:     nd.NotifBlockstore,
		toCheck:              make(chan uint, 100000),
		retrievalsInProgress: make(map[uint]*util.RetrievalProgress),
		remoteTransferStatus: cache,
		shuttleMgr:           shuttleMgr,
		inflightCids:         make(map[cid.Cid]uint),
		isDealMakingDisabled: cfg.Deal.IsDisabled,
		tracer:               otel.Tracer("replicator"),
		IncomingRPCMessages:  make(chan *drpc.Message, cfg.RPCMessage.IncomingQueueSize),
		minerManager:         minerManager,
		log:                  log,
		transferMgr:          transferMgr,
		consolidatingZones:   make(map[uint]bool),
		aggregatingZones:     make(map[uint]bool),
	}

	cm.queueMgr = newQueueManager(func(c uint) {
		cm.ToCheck(c)
	})
	return cm, nil
}
