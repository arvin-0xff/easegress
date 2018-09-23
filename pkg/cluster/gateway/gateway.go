package gateway

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexdecteam/easegateway/pkg/cluster"
	"github.com/hexdecteam/easegateway/pkg/common"
	"github.com/hexdecteam/easegateway/pkg/logger"
	"github.com/hexdecteam/easegateway/pkg/model"
	"github.com/hexdecteam/easegateway/pkg/option"

	"github.com/hashicorp/logutils"
)

type Mode string

func (m Mode) String() string {
	return string(m)
}

const (
	NilMode   Mode = ""
	WriteMode Mode = "Write"
	ReadMode  Mode = "Read"

	groupTagKey = "group"
	modeTagKey  = "mode"
)

type Config struct {
	ClusterGroup      string
	ClusterMemberMode Mode
	ClusterMemberName string
	Peers             []string

	OPLogMaxSeqGapToPull  uint16
	OPLogPullMaxCountOnce uint16
	OPLogPullInterval     time.Duration
	OPLogPullTimeout      time.Duration
}

type GatewayCluster struct {
	conf        *Config
	mod         *model.Model
	clusterConf *cluster.Config
	cluster     *cluster.Cluster
	log         *opLog
	mode        Mode

	statusLock sync.RWMutex
	stopChan   chan struct{}
	stopped    bool

	syncOpLogLock sync.Mutex

	eventStream chan cluster.Event
}

func NewGatewayCluster(conf Config, mod *model.Model) (*GatewayCluster, error) {
	if mod == nil {
		return nil, fmt.Errorf("model is nil")
	}

	switch {
	case len(conf.ClusterGroup) == 0:
		return nil, fmt.Errorf("empty group")
	case conf.OPLogMaxSeqGapToPull == 0:
		return nil, fmt.Errorf("oplog_max_seq_gap_to_pull must be greater then 0")
	case conf.OPLogPullMaxCountOnce == 0:
		return nil, fmt.Errorf("oplog_pull_max_count_once must be greater then 0")
	case conf.OPLogPullInterval == 0:
		return nil, fmt.Errorf("oplog_pull_interval must be greater than 0")
	case conf.OPLogPullTimeout.Seconds() < 10:
		return nil, fmt.Errorf("oplog_pull_timeout must be greater than or equals to 10")
	}

	eventStream := make(chan cluster.Event, 1024)

	// TODO: choose config of under layer automatically
	basisConf := cluster.DefaultLANConfig()
	basisConf.NodeName = conf.ClusterMemberName
	basisConf.NodeTags[groupTagKey] = conf.ClusterGroup
	basisConf.NodeTags[modeTagKey] = conf.ClusterMemberMode.String()
	basisConf.BindAddress = option.ClusterHost
	basisConf.AdvertiseAddress = option.ClusterHost
	basisConf.UDPBufferSize = option.PacketBufferBytes
	basisConf.GossipInterval = option.GossipInterval

	if common.StrInSlice(basisConf.AdvertiseAddress, []string{"127.0.0.1", "localhost", "0.0.0.0"}) {
		return nil, fmt.Errorf("invalid advertise address %s, it should be reachable from peer",
			basisConf.AdvertiseAddress)
	}

	var minLogLevel logutils.LogLevel
	if option.Stage == "debug" {
		minLogLevel = logutils.LogLevel("DEBUG")
	} else {
		minLogLevel = logutils.LogLevel("WARN")
	}
	basisConf.LogOutput = &logutils.LevelFilter{
		Levels:   []logutils.LogLevel{"DEBUG", "WARN", "ERROR"},
		MinLevel: minLogLevel,
		Writer:   logger.Writer(),
	}
	basisConf.EventStream = eventStream

	basis, err := cluster.Create(*basisConf)
	if err != nil {
		return nil, err
	}

	log, err := NewOPLog(filepath.Join(common.INVENTORY_HOME_DIR, "oplog"))
	if err != nil {
		return nil, err
	}

	gc := &GatewayCluster{
		conf:        &conf,
		mod:         mod,
		clusterConf: basisConf,
		cluster:     basis,
		log:         log,
		mode:        conf.ClusterMemberMode,
		stopChan:    make(chan struct{}),

		eventStream: eventStream,
	}

	go func() {
		select {
		case <-gc.stopChan:
			return
		case <-gc.cluster.Stopped():
			logger.Warnf("[stop the gateway cluster internally due to basis cluster is gone]")
			gc.internalStop(false)
		}
	}()

	go gc.dispatch()

	if len(conf.Peers) > 0 {
		logger.Infof("[start to join peer member(s) (total=%d): %s]",
			len(conf.Peers), strings.Join(conf.Peers, ", "))

		connected, err := basis.Join(conf.Peers)
		if err != nil {
			logger.Errorf("[join peer member(s) failed: %v]", err)
		} else {
			logger.Infof("[peer member(s) joined, connected to %d member(s) totally]", connected)
		}
	}

	if gc.Mode() == ReadMode {
		go gc.syncOpLogLoop()
	}

	return gc, nil
}

func (gc *GatewayCluster) NodeName() string {
	return gc.clusterConf.NodeName
}

func (gc *GatewayCluster) Mode() Mode {
	return gc.mode
}

func (gc *GatewayCluster) OPLog() *opLog {
	return gc.log
}

func (gc *GatewayCluster) Stop() error {
	return gc.internalStop(true)
}

func (gc *GatewayCluster) internalStop(stopBasis bool) error {
	gc.statusLock.Lock()
	defer gc.statusLock.Unlock()

	if gc.stopped {
		return fmt.Errorf("already stopped")
	}

	close(gc.stopChan)

	if stopBasis {
		err := gc.cluster.Leave()
		if err != nil {
			return err
		}

		for gc.cluster.NodeStatus() != cluster.NodeLeft {
			time.Sleep(100 * time.Millisecond)
		}

		err = gc.cluster.Stop()
		if err != nil {
			return err
		}
	}

	err := gc.log.Close()
	if err != nil {
		return err
	}

	gc.stopped = true

	return nil
}

func (gc *GatewayCluster) Stopped() bool {
	gc.statusLock.RLock()
	defer gc.statusLock.RUnlock()

	return gc.stopped
}

func (gc *GatewayCluster) dispatch() {
LOOP:
	for {
		select {
		case event := <-gc.eventStream:
			switch event := event.(type) {
			case *cluster.RequestEvent:
				if len(event.RequestPayload) == 0 {
					break
				}

				if event.Closed() {
					logger.Warnf("[member %s received a closed request %s, it arrives too late, ignored]",
						gc.clusterConf.NodeName, event.RequestName)
					break
				}

				switch MessageType(event.RequestPayload[0]) {
				case querySeqMessage:
					logger.Debugf("[member %s received querySeqMessage message]",
						gc.clusterConf.NodeName)

					go gc.handleQuerySequence(event)
				case queryMemberMessage:
					logger.Debugf("[member %s received queryMemberMessage message]",
						gc.clusterConf.NodeName)

					go gc.handleQueryMember(event)
				case queryMembersListMessage:
					logger.Debugf("[member %s received queryMembersListMessage message]",
						gc.clusterConf.NodeName)

					go gc.handleQueryMembersList(event)
				case queryGroupMessage:
					logger.Debugf("[member %s received queryGroupMessage message]",
						gc.clusterConf.NodeName)

					go gc.handleQueryGroup(event)
				case operationMessage:
					if gc.Mode() == WriteMode {
						logger.Debugf("[member %s received operationMessage message]",
							gc.clusterConf.NodeName)

						go gc.handleOperation(event)
					} else {
						logger.Errorf("[BUG: member with read mode received operationMessage]")
					}
				case operationRelayMessage:
					if gc.Mode() == ReadMode {
						logger.Debugf("[member %s received operationRelayMessage message]",
							gc.clusterConf.NodeName)

						go gc.handleOperationRelay(event)
					} else {
						logger.Errorf(
							"[BUG: member with write mode received operationRelayMessage]")
					}
				case retrieveMessage:
					if gc.Mode() == WriteMode {
						logger.Debugf("[member %s received retrieveMessage message]",
							gc.clusterConf.NodeName)

						go gc.handleRetrieve(event)
					} else {
						logger.Errorf("[BUG: member with read mode received retrieveMessage]")
					}
				case retrieveRelayMessage:
					if gc.Mode() == ReadMode {
						logger.Debugf("[member %s received retrieveRelayMessage message]",
							gc.clusterConf.NodeName)

						go gc.handleRetrieveRelay(event)
					} else {
						logger.Errorf(
							"[BUG: member with write mode received retrieveRelayMessage]")
					}
				case statMessage:
					logger.Debugf("[member %s received statMessage message]",
						gc.clusterConf.NodeName)

					go gc.handleStat(event)
				case statRelayMessage:
					logger.Debugf("[member %s received statRelayMessage message]",
						gc.clusterConf.NodeName)

					go gc.handleStatRelay(event)
				case opLogPullMessage:
					logger.Debugf("[member %s received opLogPullMessage message]",
						gc.clusterConf.NodeName)

					go gc.handleOPLogPull(event)
				}
			case *cluster.MemberEvent:
				switch event.Type() {
				case cluster.MemberJoinEvent:
					logger.Infof("[member %s (group=%s, mode=%s) joined to the cluster]",
						event.Member.NodeName, event.Member.NodeTags[groupTagKey],
						event.Member.NodeTags[modeTagKey])
				case cluster.MemberLeftEvent:
					logger.Infof("[member %s (group=%s, mode=%s) left from the cluster]",
						event.Member.NodeName, event.Member.NodeTags[groupTagKey],
						event.Member.NodeTags[modeTagKey])
				case cluster.MemberFailedEvent:
					logger.Warnf("[member %s (group=%s, mode=%s) failed in the cluster]",
						event.Member.NodeName, event.Member.NodeTags[groupTagKey],
						event.Member.NodeTags[modeTagKey])
				case cluster.MemberUpdateEvent:
					logger.Infof("[member %s (group=%s, mode=%s) updated in the cluster]",
						event.Member.NodeName, event.Member.NodeTags[groupTagKey],
						event.Member.NodeTags[modeTagKey])
				case cluster.MemberCleanupEvent:
					logger.Debugf("[member %s (group=%s, mode=%s) record is cleaned up]",
						event.Member.NodeName, event.Member.NodeTags[groupTagKey],
						event.Member.NodeTags[modeTagKey])
				}
			}
		case <-gc.stopChan:
			break LOOP
		}
	}
}

func (gc *GatewayCluster) localMemberName() string {
	return gc.conf.ClusterMemberName
}

func (gc *GatewayCluster) localGroupName() string {
	return gc.cluster.GetConfig().NodeTags[groupTagKey]
}

// first return parameter contains writer node
// second return parameter contains error if writer don't exist in specifc group
func (gc *GatewayCluster) writerInGroup(g string) (string, error) {
	totalMembers := gc.cluster.Members()
	for _, member := range totalMembers {
		if member.Status == cluster.MemberAlive {
			group := member.NodeTags[groupTagKey]
			nodeName := member.NodeName
			mod := Mode(member.NodeTags[modeTagKey])
			if mod == WriteMode && group == g {
				return nodeName, nil
			}
		}
	}
	return "", fmt.Errorf("writer doesn't exist in group: %s", g)
}

// choose writer first if possible, else use other node instead
// return error if no peer exist in group
func (gc *GatewayCluster) choosePeerForGroup(g string) (string, error) {
	totalMembers := gc.cluster.Members()
	var candidate string
	for _, member := range totalMembers {
		if member.Status == cluster.MemberAlive {
			group := member.NodeTags[groupTagKey]
			nodeName := member.NodeName
			mod := Mode(member.NodeTags[modeTagKey])
			if group == g {
				if mod == WriteMode {
					return nodeName, nil
				} else {
					candidate = nodeName
				}
			}
		}
	}
	if candidate == "" {
		return "", fmt.Errorf("group %s doesn't has any peer", g)
	}
	return candidate, nil
}

// first return parameter contains all writers node
// second return parameter contains error if writer don't exist in specifc group
func (gc *GatewayCluster) writerInEveryGroup() ([]string, error) {
	totalMembers := gc.cluster.Members()
	writerBook := make(map[string]string)
	groupBook := make(map[string]struct{})

	for _, member := range totalMembers {
		if member.Status == cluster.MemberAlive {
			group := member.NodeTags[groupTagKey]
			nodeName := member.NodeName
			mod := Mode(member.NodeTags[modeTagKey])
			groupBook[group] = struct{}{}
			if mod == WriteMode {
				writerBook[group] = nodeName
			}
		}
	}
	nodes := make([]string, 0, len(writerBook))
	for group := range groupBook {
		if writer, exist := writerBook[group]; exist {
			nodes = append(nodes, writer)
		} else {
			return nil, fmt.Errorf("writer doesn't exist in group: %s", group)
		}
	}
	return nodes, nil
}

func (gc *GatewayCluster) aliveNodesInCluster(mode Mode, group string) []string {
	nodes := make([]string, 0)
	totalMembers := gc.cluster.Members()
	nodesBook := make(map[string]struct{})

	for _, member := range totalMembers {
		memberMode := Mode(member.NodeTags[modeTagKey])
		if member.Status == cluster.MemberAlive &&
			(mode == NilMode || mode == memberMode) &&
			(group == NoneGroup || group == member.NodeTags[groupTagKey]) {
			if _, ok := nodesBook[member.NodeName]; ok {
				continue
			}
			nodesBook[member.NodeName] = struct{}{}
			nodes = append(nodes, member.NodeName)
		}
	}

	sort.Strings(nodes)
	return nodes
}

func (gc *GatewayCluster) groupsInCluster() []string {
	groups := make([]string, 0)
	totalMembers := gc.cluster.Members()
	groupsBook := make(map[string]struct{})

	for _, member := range totalMembers {
		group := member.NodeTags[groupTagKey]
		if _, ok := groupsBook[group]; ok {
			continue
		}
		groupsBook[group] = struct{}{}

		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}

func (gc *GatewayCluster) groupExistInCluster(group string) bool {
	groups := gc.groupsInCluster()
	exist := false
	for _, name := range groups {
		if group == name {
			exist = true
			break
		}
	}
	return exist
}

// RestAliveMembersInSameGroup is used to retrieve rest alive members which is in the same group with current node
func (gc *GatewayCluster) RestAliveMembersInSameGroup() (ret []cluster.Member) {
	totalMembers := gc.cluster.Members()

	groupName := gc.localGroupName()

	var members []string

	for _, member := range totalMembers {
		if member.NodeTags[groupTagKey] == groupName &&
			member.Status == cluster.MemberAlive &&
			member.NodeName != gc.clusterConf.NodeName {

			ret = append(ret, member)
		}

		members = append(members, fmt.Sprintf("%s (%s:%d) %s, %v",
			member.NodeName, member.Address, member.Port, member.Status.String(), member.NodeTags))
	}

	logger.Debugf("[total members in cluster (count=%d): %v]", len(members), strings.Join(members, ", "))

	return ret
}

func (gc *GatewayCluster) handleResp(req *cluster.RequestEvent, header uint8, resp interface{}) {
	payload, err := cluster.PackWithHeader(resp, header)
	if err != nil {
		logger.Errorf("[BUG: PackWithHeader %v failed: %v]", resp, err)
		return
	}

	err = req.Respond(payload)
	if err != nil {
		logger.Errorf("[respond to request %s, node %s failed: %v]",
			req.RequestName, req.RequestNodeName, err)
	}
}

// recordResp just records known response of member and ignore others.
// It does its best to record response, and just exits when GatewayCluster stopped
// or future got timeout, the caller could check membersRespBook to get the result.
// it may failed(timeout) to receive response from members.
func (gc *GatewayCluster) recordResp(requestName string, future *cluster.Future, membersRespBook map[string][]byte) {
	memberRespCount := 0
LOOP:
	for memberRespCount < len(membersRespBook) {
		select {
		case memberResp, ok := <-future.Response():
			memberRespCount++
			if !ok { // timeout
				continue LOOP // collect response from other member
			}

			payload, known := membersRespBook[memberResp.ResponseNodeName]
			if !known {
				logger.Warnf("[received the response from an unexpected node %s "+
					"started during the request %s]", memberResp.ResponseNodeName,
					fmt.Sprintf("%s_relayed", requestName))
				continue LOOP
			}

			if payload != nil {
				logger.Errorf("[received multiple responses from node %s "+
					"for request %s, skipped. probably need to tune cluster configuration]",
					memberResp.ResponseNodeName, fmt.Sprintf("%s_relayed", requestName))
				continue LOOP
			}

			if memberResp.Payload == nil {
				logger.Errorf("[BUG: received empty response from node %s for request %s]",
					memberResp.ResponseNodeName, fmt.Sprintf("%s_relayed", requestName))
				memberResp.Payload = []byte("")
			}

			membersRespBook[memberResp.ResponseNodeName] = memberResp.Payload
		case <-gc.stopChan:
			break LOOP
		}
	}

	return
}