//go:build !windows

// Copyright (c) 2021-2022 Tigera, Inc. All rights reserved.
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

package intdataplane

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"regexp"
	"strconv"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/felix/bpf"
	"github.com/projectcalico/calico/felix/bpf/asm"
	"github.com/projectcalico/calico/felix/bpf/bpfmap"
	"github.com/projectcalico/calico/felix/bpf/conntrack"
	"github.com/projectcalico/calico/felix/bpf/counters"
	"github.com/projectcalico/calico/felix/bpf/hook"
	"github.com/projectcalico/calico/felix/bpf/ifstate"
	bpfipsets "github.com/projectcalico/calico/felix/bpf/ipsets"
	"github.com/projectcalico/calico/felix/bpf/jump"
	"github.com/projectcalico/calico/felix/bpf/maps"
	bpfmaps "github.com/projectcalico/calico/felix/bpf/maps"
	"github.com/projectcalico/calico/felix/bpf/mock"
	"github.com/projectcalico/calico/felix/bpf/polprog"
	"github.com/projectcalico/calico/felix/bpf/state"
	"github.com/projectcalico/calico/felix/bpf/tc"
	"github.com/projectcalico/calico/felix/bpf/xdp"
	"github.com/projectcalico/calico/felix/environment"
	"github.com/projectcalico/calico/felix/idalloc"
	"github.com/projectcalico/calico/felix/ifacemonitor"
	"github.com/projectcalico/calico/felix/ip"
	"github.com/projectcalico/calico/felix/ipsets"
	"github.com/projectcalico/calico/felix/logutils"
	"github.com/projectcalico/calico/felix/proto"
	"github.com/projectcalico/calico/felix/rules"
	"github.com/projectcalico/calico/libcalico-go/lib/set"
)

type mockDataplane struct {
	mutex      sync.Mutex
	lastProgID int
	progs      map[string]int
	policy     map[string]polprog.Rules
	routes     map[ip.CIDR]struct{}

	ensureStartedFn    func()
	ensureQdiscFn      func(string) (bool, error)
	interfaceByIndexFn func(ifindex int) (*net.Interface, error)
}

func newMockDataplane() *mockDataplane {
	return &mockDataplane{
		lastProgID: 5,
		progs:      map[string]int{},
		policy:     map[string]polprog.Rules{},
		routes:     map[ip.CIDR]struct{}{},
	}
}

func (m *mockDataplane) ensureStarted() {
	if m.ensureStartedFn != nil {
		m.ensureStartedFn()
	}
}

func (m *mockDataplane) interfaceByIndex(ifindex int) (*net.Interface, error) {
	if m.interfaceByIndexFn != nil {
		return m.interfaceByIndexFn(ifindex)
	}

	return nil, errors.New("no such network interface")
}

func (m *mockDataplane) ensureBPFDevices() error {
	return nil
}

func (m *mockDataplane) loadDefaultPolicies() error {
	return nil
}

func (m *mockDataplane) ensureProgramAttached(ap attachPoint) (bpf.AttachResult, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	var res tc.AttachResult // we don't care about the values

	if apxdp, ok := ap.(*xdp.AttachPoint); ok {
		apxdp.HookLayout = hook.Layout{
			hook.SubProgXDPAllowed: 123,
			hook.SubProgXDPDrop:    456,
		}
	}

	key := ap.IfaceName() + ":" + ap.HookName().String()
	if _, exists := m.progs[key]; exists {
		return res, nil
	}
	m.lastProgID += 1
	m.progs[key] = m.lastProgID
	return res, nil
}

func (m *mockDataplane) ensureNoProgram(ap attachPoint) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	key := ap.IfaceName() + ":" + ap.HookName().String()
	if _, exists := m.progs[key]; exists {
		delete(m.policy, key)
		delete(m.progs, key)
	}
	return nil
}

func (m *mockDataplane) ensureQdisc(iface string) (bool, error) {
	if m.ensureQdiscFn != nil {
		return m.ensureQdiscFn(iface)
	}
	return false, nil
}

func (m *mockDataplane) updatePolicyProgram(rules polprog.Rules, polDir string, ap attachPoint) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	key := ap.IfaceName() + ":" + ap.HookName().String()
	m.policy[key] = rules
	return nil
}

func (m *mockDataplane) removePolicyProgram(ap attachPoint) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	key := ap.IfaceName() + ":" + ap.HookName().String()
	delete(m.policy, key)
	return nil
}

func (m *mockDataplane) setAcceptLocal(iface string, val bool) error {
	return nil
}

func (m *mockDataplane) setRPFilter(iface string, val int) error {
	return nil
}

func (m *mockDataplane) getRules(key string) *polprog.Rules {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if _, ok := m.progs[key]; ok {
		rules, exist := m.policy[key]
		if exist {
			return &rules
		}
	}
	return nil
}

func (m *mockDataplane) setAndReturn(vari **polprog.Rules, key string) func() *polprog.Rules {
	return func() *polprog.Rules {
		*vari = m.getRules(key)
		return *vari
	}
}

func (m *mockDataplane) programAttached(key string) bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.progs[key] != 0
}

func (m *mockDataplane) setRoute(cidr ip.CIDR) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.routes[cidr] = struct{}{}
}

func (m *mockDataplane) delRoute(cidr ip.CIDR) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	delete(m.routes, cidr)
}

func (m *mockDataplane) ruleMatchID(dir, action, owner, name string, idx int) polprog.RuleMatchID {
	h := fnv.New64a()
	h.Write([]byte(action + owner + dir + strconv.Itoa(idx+1) + name))
	return h.Sum64()
}

func (m *mockDataplane) queryClassifier(ifindex, handle, prio int, ingress bool) (int, error) {
	return 0, nil
}

var (
	fdCounterLock sync.Mutex
	fdCounter     = uint32(1234)
)

func (m *mockDataplane) loadTCLogFilter(ap *tc.AttachPoint) (fileDescriptor, int, error) {
	fdCounterLock.Lock()
	defer fdCounterLock.Unlock()
	fdCounter++
	return mockFD(fdCounter), ap.LogFilterIdx, nil
}

type mockProgMapDP struct {
	*mockDataplane
}

func (m *mockProgMapDP) loadPolicyProgram(progName string,
	ipFamily proto.IPVersion,
	rules polprog.Rules,
	staticProgsMap maps.Map,
	polProgsMap maps.Map,
	opts ...polprog.Option,
) ([]fileDescriptor, []asm.Insns, error) {
	fdCounterLock.Lock()
	defer fdCounterLock.Unlock()
	fdCounter++

	return []fileDescriptor{mockFD(fdCounter)}, []asm.Insns{{{Comments: []string{"blah"}}}}, nil
}

var _ hasLoadPolicyProgram = (*mockProgMapDP)(nil)

type mockFD uint32

func (f mockFD) Close() error {
	log.WithField("fd", int(f)).Debug("Closing mockFD")
	return nil
}

func (f mockFD) FD() uint32 {
	return uint32(f)
}

var _ = Describe("BPF Endpoint Manager", func() {

	var (
		bpfEpMgr             *bpfEndpointManager
		dp                   *mockDataplane
		mockDP               bpfDataplane
		fibLookupEnabled     bool
		endpointToHostAction string
		dataIfacePattern     string
		workloadIfaceRegex   string
		ipSetIDAllocator     *idalloc.IDAllocator
		vxlanMTU             int
		nodePortDSR          bool
		maps                 *bpfmap.Maps
		rrConfigNormal       rules.Config
		ruleRenderer         rules.RuleRenderer
		filterTableV4        IptablesTable
		ifStateMap           *mock.Map
		countersMap          *mock.Map
		jumpMap              *mock.Map
		xdpJumpMap           *mock.Map
	)

	BeforeEach(func() {
		fibLookupEnabled = true
		endpointToHostAction = "DROP"
		dataIfacePattern = "^eth0"
		workloadIfaceRegex = "cali"
		ipSetIDAllocator = idalloc.New()
		vxlanMTU = 0
		nodePortDSR = true

		bpfmaps.EnableRepin()

		maps = new(bpfmap.Maps)

		maps.IpsetsMap = bpfipsets.Map()
		maps.StateMap = state.Map()
		maps.CtMap = conntrack.Map()
		ifStateMap = mock.NewMockMap(ifstate.MapParams)
		maps.IfStateMap = ifStateMap
		cparams := counters.MapParameters
		cparams.ValueSize *= bpfmaps.NumPossibleCPUs()
		countersMap = mock.NewMockMap(cparams)
		maps.CountersMap = countersMap
		maps.RuleCountersMap = mock.NewMockMap(counters.PolicyMapParameters)

		progsParams := bpfmaps.MapParameters{
			Type:       "prog_array",
			KeySize:    4,
			ValueSize:  4,
			MaxEntries: 1000,
			Name:       "cali_progs",
			Version:    2,
		}

		maps.ProgramsMap = mock.NewMockMap(progsParams)
		maps.XDPProgramsMap = mock.NewMockMap(progsParams)
		jumpMap = mock.NewMockMap(progsParams)
		maps.JumpMap = jumpMap
		xdpJumpMap = mock.NewMockMap(progsParams)
		maps.XDPJumpMap = xdpJumpMap

		rrConfigNormal = rules.Config{
			IPIPEnabled:                 true,
			IPIPTunnelAddress:           nil,
			IPSetConfigV4:               ipsets.NewIPVersionConfig(ipsets.IPFamilyV4, "cali", nil, nil),
			IPSetConfigV6:               ipsets.NewIPVersionConfig(ipsets.IPFamilyV6, "cali", nil, nil),
			IptablesMarkAccept:          0x8,
			IptablesMarkPass:            0x10,
			IptablesMarkScratch0:        0x20,
			IptablesMarkScratch1:        0x40,
			IptablesMarkEndpoint:        0xff00,
			IptablesMarkNonCaliEndpoint: 0x0100,
			KubeIPVSSupportEnabled:      true,
			WorkloadIfacePrefixes:       []string{"cali", "tap"},
			VXLANPort:                   4789,
			VXLANVNI:                    4096,
		}
		ruleRenderer = rules.NewRenderer(rrConfigNormal)
		filterTableV4 = newMockTable("filter")
	})

	AfterEach(func() {
		bpfmaps.DisableRepin()
	})

	newBpfEpMgr := func() {
		var err error
		bpfEpMgr, err = newBPFEndpointManager(
			mockDP,
			&Config{
				Hostname:              "uthost",
				BPFLogLevel:           "info",
				BPFDataIfacePattern:   regexp.MustCompile(dataIfacePattern),
				VXLANMTU:              vxlanMTU,
				VXLANPort:             rrConfigNormal.VXLANPort,
				BPFNodePortDSREnabled: nodePortDSR,
				RulesConfig: rules.Config{
					EndpointToHostAction: endpointToHostAction,
				},
				BPFExtToServiceConnmark: 0,
				BPFHostNetworkedNAT:     "Enabled",
				BPFPolicyDebugEnabled:   true,
			},
			maps,
			fibLookupEnabled,
			regexp.MustCompile(workloadIfaceRegex),
			ipSetIDAllocator,
			ruleRenderer,
			filterTableV4,
			nil,
			logutils.NewSummarizer("test"),
			&environment.FakeFeatureDetector{},
		)
		Expect(err).NotTo(HaveOccurred())
		bpfEpMgr.Features = environment.NewFeatureDetector(nil).GetFeatures()
		bpfEpMgr.hostIP = net.ParseIP("1.2.3.4")
	}

	genIfaceUpdate := func(name string, state ifacemonitor.State, index int) func() {
		return func() {
			bpfEpMgr.OnUpdate(&ifaceStateUpdate{Name: name, State: state, Index: index})
			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			if state == ifacemonitor.StateUp && (bpfEpMgr.isDataIface(name) || bpfEpMgr.isWorkloadIface(name)) {
				ExpectWithOffset(1, ifStateMap.ContainsKey(ifstate.NewKey(uint32(index)).AsBytes())).To(BeTrue())
			}
		}
	}

	genHEPUpdate := func(heps ...interface{}) func() {
		return func() {
			hostIfaceToEp := make(map[string]proto.HostEndpoint)
			for i := 0; i < len(heps); i += 2 {
				log.Infof("%v = %v", heps[i], heps[i+1])
				hostIfaceToEp[heps[i].(string)] = heps[i+1].(proto.HostEndpoint)
			}
			log.Infof("2 hostIfaceToEp = %v", hostIfaceToEp)
			bpfEpMgr.OnHEPUpdate(hostIfaceToEp)
			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
		}
	}

	genPolicy := func(tier, policy string) func() {
		return func() {
			bpfEpMgr.OnUpdate(&proto.ActivePolicyUpdate{
				Id:     &proto.PolicyID{Tier: tier, Name: policy},
				Policy: &proto.Policy{},
			})
			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
		}
	}

	genUntracked := func(tier, policy string) func() {
		return func() {
			bpfEpMgr.OnUpdate(&proto.ActivePolicyUpdate{
				Id:     &proto.PolicyID{Tier: tier, Name: policy},
				Policy: &proto.Policy{Untracked: true},
			})
			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
		}
	}

	genWLUpdate := func(name string, policies ...string) func() {
		return func() {
			update := &proto.WorkloadEndpointUpdate{
				Id: &proto.WorkloadEndpointID{
					OrchestratorId: "k8s",
					WorkloadId:     name,
					EndpointId:     name,
				},
				Endpoint: &proto.WorkloadEndpoint{Name: name},
			}
			if len(policies) > 0 {
				update.Endpoint.Tiers = []*proto.TierInfo{{
					Name:            "default",
					IngressPolicies: policies,
					EgressPolicies:  policies,
				}}
			}
			bpfEpMgr.OnUpdate(update)
			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
		}
	}

	hostEp := proto.HostEndpoint{
		Name: "uthost-eth0",
		PreDnatTiers: []*proto.TierInfo{
			&proto.TierInfo{
				Name:            "default",
				IngressPolicies: []string{"mypolicy"},
			},
		},
	}

	hostEpNorm := proto.HostEndpoint{
		Name: "uthost-eth0",
		Tiers: []*proto.TierInfo{
			&proto.TierInfo{
				Name:            "default",
				IngressPolicies: []string{"mypolicy"},
				EgressPolicies:  []string{"mypolicy"},
			},
		},
	}

	JustBeforeEach(func() {
		dp = newMockDataplane()
		mockDP = dp
		newBpfEpMgr()
	})

	It("exists", func() {
		Expect(bpfEpMgr).NotTo(BeNil())
	})

	It("does not have HEP in initial state", func() {
		Expect(bpfEpMgr.hostIfaceToEpMap["eth0"]).NotTo(Equal(hostEp))
	})

	Context("with workload and host-* endpoints", func() {
		JustBeforeEach(func() {
			genPolicy("default", "mypolicy")()
			genIfaceUpdate("eth0", ifacemonitor.StateUp, 10)()
			genWLUpdate("cali12345")()
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genHEPUpdate(allInterfaces, hostEpNorm)()
		})

		It("does not have host-* policy on the workload interface", func() {
			var eth0I, eth0E, eth0X, caliI, caliE *polprog.Rules

			// Check eth0 ingress.
			Eventually(dp.setAndReturn(&eth0I, "eth0:ingress")).ShouldNot(BeNil())
			Expect(eth0I.ForHostInterface).To(BeTrue())
			Expect(eth0I.HostNormalTiers).To(HaveLen(1))
			Expect(eth0I.HostNormalTiers[0].Policies).To(HaveLen(1))
			Expect(eth0I.SuppressNormalHostPolicy).To(BeFalse())

			// Check eth0 egress.
			Eventually(dp.setAndReturn(&eth0E, "eth0:egress")).ShouldNot(BeNil())
			Expect(eth0E.ForHostInterface).To(BeTrue())
			Expect(eth0E.HostNormalTiers).To(HaveLen(1))
			Expect(eth0E.HostNormalTiers[0].Policies).To(HaveLen(1))
			Expect(eth0E.SuppressNormalHostPolicy).To(BeFalse())

			// Check workload ingress.
			Eventually(dp.setAndReturn(&caliI, "cali12345:egress")).ShouldNot(BeNil())
			Expect(caliI.ForHostInterface).To(BeFalse())
			Expect(caliI.SuppressNormalHostPolicy).To(BeTrue())

			// Check workload egress.
			Eventually(dp.setAndReturn(&caliE, "cali12345:ingress")).ShouldNot(BeNil())
			Expect(caliE.ForHostInterface).To(BeFalse())
			Expect(caliE.SuppressNormalHostPolicy).To(BeTrue())

			// Check no XDP.
			Eventually(dp.setAndReturn(&eth0X, "eth0:xdp")).Should(BeNil())
		})

		Context("with DefaultEndpointToHostAction RETURN", func() {
			BeforeEach(func() {
				endpointToHostAction = "RETURN"
			})

			It("has host-* policy on workload egress but not ingress", func() {
				var caliI, caliE *polprog.Rules

				// Check workload ingress.
				Eventually(dp.setAndReturn(&caliI, "cali12345:egress")).ShouldNot(BeNil())
				Expect(caliI.ForHostInterface).To(BeFalse())
				Expect(caliI.SuppressNormalHostPolicy).To(BeTrue())

				// Check workload egress.
				Eventually(dp.setAndReturn(&caliE, "cali12345:ingress")).ShouldNot(BeNil())
				Expect(caliE.ForHostInterface).To(BeFalse())
				Expect(caliE.HostNormalTiers).To(HaveLen(1))
				Expect(caliE.HostNormalTiers[0].Policies).To(HaveLen(1))
				Expect(caliE.SuppressNormalHostPolicy).To(BeFalse())
			})
		})
	})

	Context("with eth0 up", func() {
		JustBeforeEach(func() {
			genPolicy("default", "mypolicy")()
			genIfaceUpdate("eth0", ifacemonitor.StateUp, 10)()
		})

		It("should attach to eth0", func() {
			Expect(dp.programAttached("eth0:ingress")).To(BeTrue())
			Expect(dp.programAttached("eth0:egress")).To(BeTrue())
		})

		Context("with dataIfacePattern changed to eth1", func() {
			JustBeforeEach(func() {
				dataIfacePattern = "^eth1"
				newBpfEpMgr()

				dp.ensureStartedFn = func() {
					bpfEpMgr.initAttaches = map[string]bpf.EPAttachInfo{
						"eth0": {Ingress: 12345},
					}
				}

			})

			It("should detach from eth0 when eth0 up before first CompleteDeferredWork()", func() {
				Expect(dp.programAttached("eth0:ingress")).To(BeTrue())
				Expect(dp.programAttached("eth0:egress")).To(BeTrue())

				genIfaceUpdate("eth0", ifacemonitor.StateUp, 10)()
				genIfaceUpdate("eth1", ifacemonitor.StateUp, 11)()

				err := bpfEpMgr.CompleteDeferredWork()
				Expect(err).NotTo(HaveOccurred())

				// We inherited dp from the previous bpfEpMgr and it has eth0
				// attached. This should clean it up.
				Expect(dp.programAttached("eth0:ingress")).To(BeFalse())
				Expect(dp.programAttached("eth0:egress")).To(BeFalse())

				Expect(dp.programAttached("eth1:ingress")).To(BeTrue())
				Expect(dp.programAttached("eth1:egress")).To(BeTrue())
			})

			It("should detach from eth0 when eth0 up after first CompleteDeferredWork()", func() {
				Expect(dp.programAttached("eth0:ingress")).To(BeTrue())
				Expect(dp.programAttached("eth0:egress")).To(BeTrue())

				genIfaceUpdate("eth1", ifacemonitor.StateUp, 11)()

				err := bpfEpMgr.CompleteDeferredWork()
				Expect(err).NotTo(HaveOccurred())

				// We inherited dp from the previous bpfEpMgr and it has eth0
				// attached. We should see it.
				Expect(dp.programAttached("eth0:ingress")).To(BeTrue())
				Expect(dp.programAttached("eth0:egress")).To(BeTrue())

				Expect(dp.programAttached("eth1:ingress")).To(BeTrue())
				Expect(dp.programAttached("eth1:egress")).To(BeTrue())

				genIfaceUpdate("eth0", ifacemonitor.StateUp, 10)()

				err = bpfEpMgr.CompleteDeferredWork()
				Expect(err).NotTo(HaveOccurred())

				// We inherited dp from the previous bpfEpMgr and it has eth0
				// attached. This should clean it up.
				Expect(dp.programAttached("eth0:ingress")).To(BeFalse())
				Expect(dp.programAttached("eth0:egress")).To(BeFalse())

				Expect(dp.programAttached("eth1:ingress")).To(BeTrue())
				Expect(dp.programAttached("eth1:egress")).To(BeTrue())
			})
		})

		Context("with eth0 host endpoint", func() {
			JustBeforeEach(genHEPUpdate("eth0", hostEp))

			It("stores host endpoint for eth0", func() {
				Expect(bpfEpMgr.hostIfaceToEpMap["eth0"]).To(Equal(hostEp))
				Expect(bpfEpMgr.policiesToWorkloads[proto.PolicyID{
					Tier: "default",
					Name: "mypolicy",
				}]).To(HaveKey("eth0"))

				var eth0I, eth0E, eth0X *polprog.Rules

				// Check ingress rules.
				Eventually(dp.setAndReturn(&eth0I, "eth0:ingress")).ShouldNot(BeNil())
				Expect(eth0I.ForHostInterface).To(BeTrue())
				Expect(eth0I.HostPreDnatTiers).To(HaveLen(1))
				Expect(eth0I.HostPreDnatTiers[0].Policies).To(HaveLen(1))

				// Check egress rules.
				Eventually(dp.setAndReturn(&eth0E, "eth0:egress")).ShouldNot(BeNil())
				Expect(eth0E.ForHostInterface).To(BeTrue())
				Expect(eth0E.HostPreDnatTiers).To(BeNil())

				// Check no XDP.
				Eventually(dp.setAndReturn(&eth0X, "eth0:xdp")).Should(BeNil())

				By("adding untracked policy")
				genUntracked("default", "untracked1")()
				newHEP := hostEp
				newHEP.UntrackedTiers = []*proto.TierInfo{{
					Name:            "default",
					IngressPolicies: []string{"untracked1"},
				}}
				genHEPUpdate("eth0", newHEP)()

				// Check XDP.
				Eventually(dp.setAndReturn(&eth0X, "eth0:xdp")).ShouldNot(BeNil())
				Expect(eth0X.ForHostInterface).To(BeTrue())
				Expect(eth0X.ForXDP).To(BeTrue())
				Expect(eth0X.HostNormalTiers).To(HaveLen(1))
				Expect(eth0X.HostNormalTiers[0].Policies).To(HaveLen(1))

				By("removing untracked policy again")
				genHEPUpdate("eth0", hostEp)()

				// Check no XDP.
				Eventually(dp.setAndReturn(&eth0X, "eth0:xdp")).Should(BeNil())
			})
		})

		Context("with host-* endpoint", func() {
			JustBeforeEach(genHEPUpdate(allInterfaces, hostEp))

			It("stores host endpoint for eth0", func() {
				Expect(bpfEpMgr.hostIfaceToEpMap["eth0"]).To(Equal(hostEp))
				Expect(bpfEpMgr.policiesToWorkloads[proto.PolicyID{
					Tier: "default",
					Name: "mypolicy",
				}]).To(HaveKey("eth0"))
			})
		})
	})

	Context("with eth0 host endpoint", func() {
		JustBeforeEach(func() {
			genPolicy("default", "mypolicy")()
			genHEPUpdate("eth0", hostEp)()
		})

		Context("with eth0 up", func() {
			JustBeforeEach(genIfaceUpdate("eth0", ifacemonitor.StateUp, 10))

			It("stores host endpoint for eth0", func() {
				Expect(bpfEpMgr.hostIfaceToEpMap["eth0"]).To(Equal(hostEp))
				Expect(bpfEpMgr.policiesToWorkloads[proto.PolicyID{
					Tier: "default",
					Name: "mypolicy",
				}]).To(HaveKey("eth0"))
			})
		})
	})

	Context("with host-* endpoint", func() {
		JustBeforeEach(func() {
			genPolicy("default", "mypolicy")()
			genHEPUpdate(allInterfaces, hostEp)()
		})

		Context("with eth0 up", func() {
			JustBeforeEach(genIfaceUpdate("eth0", ifacemonitor.StateUp, 10))

			It("stores host endpoint for eth0", func() {
				Expect(bpfEpMgr.hostIfaceToEpMap["eth0"]).To(Equal(hostEp))
				Expect(bpfEpMgr.policiesToWorkloads[proto.PolicyID{
					Tier: "default",
					Name: "mypolicy",
				}]).To(HaveKey("eth0"))
			})

			Context("with eth0 down", func() {
				JustBeforeEach(genIfaceUpdate("eth0", ifacemonitor.StateDown, 10))

				It("clears host endpoint for eth0", func() {
					Expect(bpfEpMgr.hostIfaceToEpMap).To(BeEmpty())
					Expect(bpfEpMgr.policiesToWorkloads[proto.PolicyID{
						Tier: "default",
						Name: "mypolicy",
					}]).NotTo(HaveKey("eth0"))
				})
			})
			Context("with eth0 deleted", func() {
				JustBeforeEach(genIfaceUpdate("eth0", ifacemonitor.StateNotPresent, 10))

				It("clears host endpoint for eth0", func() {
					Expect(bpfEpMgr.hostIfaceToEpMap).To(BeEmpty())
					Expect(bpfEpMgr.policiesToWorkloads[proto.PolicyID{
						Tier: "default",
						Name: "mypolicy",
					}]).NotTo(HaveKey("eth0"))
				})
			})
		})
	})

	Describe("polCounters", func() {
		It("should update the maps with ruleIds", func() {
			ingRule := &proto.Rule{Action: "Allow", RuleId: "INGRESSALLOW1234"}
			egrRule := &proto.Rule{Action: "Allow", RuleId: "EGRESSALLOW12345"}
			ingRuleMatchId := bpfEpMgr.dp.ruleMatchID("Ingress", "Allow", "Policy", "allowPol", 0)
			egrRuleMatchId := bpfEpMgr.dp.ruleMatchID("Egress", "Allow", "Policy", "allowPol", 0)
			k := make([]byte, 8)
			v := make([]byte, 8)
			rcMap := bpfEpMgr.bpfmaps.RuleCountersMap

			// create a new policy
			bpfEpMgr.OnUpdate(&proto.ActivePolicyUpdate{
				Id:     &proto.PolicyID{Tier: "default", Name: "allowPol"},
				Policy: &proto.Policy{InboundRules: []*proto.Rule{ingRule}, OutboundRules: []*proto.Rule{egrRule}},
			})
			Expect(bpfEpMgr.polNameToMatchIDs).To(HaveLen(1))
			val := bpfEpMgr.polNameToMatchIDs["allowPol"]
			Expect(val.Contains(ingRuleMatchId)).To(BeTrue())
			Expect(val.Contains(egrRuleMatchId)).To(BeTrue())
			binary.LittleEndian.PutUint64(k, ingRuleMatchId)
			binary.LittleEndian.PutUint64(v, uint64(10))
			err := rcMap.Update(k[:], v[:])
			Expect(err).NotTo(HaveOccurred())
			binary.LittleEndian.PutUint64(k, egrRuleMatchId)
			err = rcMap.Update(k[:], v[:])
			Expect(err).NotTo(HaveOccurred())

			// update the ingress rule of the policy
			ingDenyRule := &proto.Rule{Action: "Deny", RuleId: "INGRESSDENY12345"}
			ingDenyRuleMatchId := bpfEpMgr.dp.ruleMatchID("Ingress", "Deny", "Policy", "allowPol", 0)
			bpfEpMgr.OnUpdate(&proto.ActivePolicyUpdate{
				Id:     &proto.PolicyID{Tier: "default", Name: "allowPol"},
				Policy: &proto.Policy{InboundRules: []*proto.Rule{ingDenyRule}, OutboundRules: []*proto.Rule{egrRule}},
			})
			Expect(bpfEpMgr.polNameToMatchIDs).To(HaveLen(1))
			val = bpfEpMgr.polNameToMatchIDs["allowPol"]
			Expect(val.Contains(ingDenyRuleMatchId)).To(BeTrue())
			Expect(val.Contains(egrRuleMatchId)).To(BeTrue())
			Expect(bpfEpMgr.dirtyRules.Contains(ingRuleMatchId)).To(BeTrue())
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(bpfEpMgr.dirtyRules.Contains(ingRuleMatchId)).NotTo(BeTrue())
			binary.LittleEndian.PutUint64(k, ingRuleMatchId)
			_, err = rcMap.Get(k)
			Expect(err).To(HaveOccurred())
			binary.LittleEndian.PutUint64(k, egrRuleMatchId)
			v, err = rcMap.Get(k)
			Expect(err).NotTo(HaveOccurred())
			Expect(binary.LittleEndian.Uint64(v)).To(Equal(uint64(10)))

			// delete the policy
			bpfEpMgr.OnUpdate(&proto.ActivePolicyRemove{Id: &proto.PolicyID{Tier: "default", Name: "allowPol"}})
			Expect(bpfEpMgr.dirtyRules.Contains(egrRuleMatchId)).To(BeTrue())
			Expect(bpfEpMgr.dirtyRules.Contains(ingDenyRuleMatchId)).To(BeTrue())
			Expect(bpfEpMgr.polNameToMatchIDs).To(HaveLen(0))
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			binary.LittleEndian.PutUint64(k, ingRuleMatchId)
			_, err = rcMap.Get(k)
			Expect(err).To(HaveOccurred())
			binary.LittleEndian.PutUint64(k, egrRuleMatchId)
			_, err = rcMap.Get(k)
			Expect(err).To(HaveOccurred())
			binary.LittleEndian.PutUint64(k, ingDenyRuleMatchId)
			_, err = rcMap.Get(k)
			Expect(err).To(HaveOccurred())

		})

		It("should cleanup the bpf map after restart", func() {
			ingRuleMatchId := bpfEpMgr.dp.ruleMatchID("Ingress", "Allow", "Policy", "allowPol", 0)
			egrRuleMatchId := bpfEpMgr.dp.ruleMatchID("Egress", "Allow", "Policy", "allowPol", 0)
			k := make([]byte, 8)
			v := make([]byte, 8)
			rcMap := bpfEpMgr.bpfmaps.RuleCountersMap

			binary.LittleEndian.PutUint64(k, ingRuleMatchId)
			binary.LittleEndian.PutUint64(v, uint64(10))
			err := rcMap.Update(k[:], v[:])
			Expect(err).NotTo(HaveOccurred())

			binary.LittleEndian.PutUint64(k, egrRuleMatchId)
			binary.LittleEndian.PutUint64(v, uint64(10))
			err = rcMap.Update(k[:], v[:])
			Expect(err).NotTo(HaveOccurred())

			newBpfEpMgr()
			binary.LittleEndian.PutUint64(k, ingRuleMatchId)
			_, err = rcMap.Get(k[:])
			Expect(err).To(HaveOccurred())
			binary.LittleEndian.PutUint64(k, egrRuleMatchId)
			_, err = rcMap.Get(k[:])
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("bpfnatip", func() {
		It("should program the routes reflecting service state", func() {
			bpfEpMgr.OnUpdate(&proto.ServiceUpdate{
				Name:      "service",
				Namespace: "test",
				ClusterIp: "1.2.3.4",
			})
			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.routes).To(HaveLen(1))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("1.2.3.4")))

			bpfEpMgr.OnUpdate(&proto.ServiceUpdate{
				Name:           "service",
				Namespace:      "test",
				ClusterIp:      "1.2.3.4",
				LoadbalancerIp: "5.6.7.8",
			})
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.routes).To(HaveLen(2))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("1.2.3.4")))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("5.6.7.8")))

			bpfEpMgr.OnUpdate(&proto.ServiceUpdate{
				Name:           "service",
				Namespace:      "test",
				ClusterIp:      "1.2.3.4",
				LoadbalancerIp: "5.6.7.8",
				ExternalIps:    []string{"1.0.0.1", "1.0.0.2"},
			})
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.routes).To(HaveLen(2))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("1.2.3.4")))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("5.6.7.8")))

			bpfEpMgr.OnUpdate(&proto.ServiceUpdate{
				Name:      "service",
				Namespace: "test",
				ClusterIp: "1.2.3.4",
			})
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.routes).To(HaveLen(1))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("1.2.3.4")))

			bpfEpMgr.OnUpdate(&proto.ServiceRemove{
				Name:      "service",
				Namespace: "test",
			})
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.routes).To(HaveLen(0))

			bpfEpMgr.OnUpdate(&proto.ServiceUpdate{
				Name:           "service",
				Namespace:      "test",
				ClusterIp:      "1.2.3.4",
				LoadbalancerIp: "5.6.7.8",
			})
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.routes).To(HaveLen(2))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("1.2.3.4")))
			Expect(dp.routes).To(HaveKey(ip.MustParseCIDROrIP("5.6.7.8")))

			bpfEpMgr.OnUpdate(&proto.ServiceRemove{
				Name:      "service",
				Namespace: "test",
			})
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.routes).To(HaveLen(0))
		})
	})

	Describe("counters", func() {
		It("should clean up after restart", func() {
			err := counters.EnsureExists(countersMap, 12345, hook.Egress)
			Expect(err).NotTo(HaveOccurred())
			err = counters.EnsureExists(countersMap, 12345, hook.Ingress)
			Expect(err).NotTo(HaveOccurred())
			err = counters.EnsureExists(countersMap, 12345, hook.XDP)
			Expect(err).NotTo(HaveOccurred())
			err = counters.EnsureExists(countersMap, 54321, hook.Egress)
			Expect(err).NotTo(HaveOccurred())
			err = counters.EnsureExists(countersMap, 54321, hook.Ingress)
			Expect(err).NotTo(HaveOccurred())

			Expect(countersMap.Contents).To(HaveLen(5))

			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genWLUpdate("cali12345")()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(countersMap.Contents).To(HaveLen(0))
			// The BPF programs will create the counters the first time they
		})

		It("should GC counters when dev goes down", func() {
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 12345)()
			genWLUpdate("cali12345")()

			err := counters.EnsureExists(countersMap, 12345, hook.Egress)
			Expect(err).NotTo(HaveOccurred())
			err = counters.EnsureExists(countersMap, 12345, hook.Ingress)
			Expect(err).NotTo(HaveOccurred())

			Expect(countersMap.Contents).To(HaveLen(2))

			genIfaceUpdate("cali12345", ifacemonitor.StateDown, 12345)()
			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(countersMap.Contents).To(HaveLen(0))
		})
	})

	Describe("ifstate", func() {
		checkIfState := func(idx int, name string, flags uint32) {
			k := ifstate.NewKey(uint32(idx))
			vb, err := ifStateMap.Get(k.AsBytes())
			if err != nil {
				Fail(fmt.Sprintf("Ifstate does not have key %s", k), 1)
			}
			vv := ifstate.ValueFromBytes(vb)
			Expect(flags).To(Equal(vv.Flags()))
			Expect(name).To(Equal(vv.IfName()))
		}

		It("should clean up jump map entries for missing interfaces", func() {
			for i := 0; i < 17; i++ {
				_ = jumpMap.Update(jump.Key(i), jump.Value(uint32(1000+i)))
				_ = jumpMap.Update(jump.Key(i+jump.TCMaxEntryPoints), jump.Value(uint32(1000+i)))
			}
			for i := 0; i < 5; i++ {
				_ = xdpJumpMap.Update(jump.Key(i), jump.Value(uint32(2000+i)))
			}

			_ = ifStateMap.Update(
				ifstate.NewKey(123).AsBytes(),
				ifstate.NewValue(ifstate.FlgReady, "eth123",
					1, 1, 2, 3, 4).AsBytes(),
			)
			_ = ifStateMap.Update(
				ifstate.NewKey(124).AsBytes(),
				ifstate.NewValue(0, "eth124",
					2, 5, 6, 7, 8).AsBytes(),
			)
			_ = ifStateMap.Update(
				ifstate.NewKey(125).AsBytes(),
				ifstate.NewValue(ifstate.FlgWEP|ifstate.FlgReady, "eth125",
					3, 9, 10, 11, 12).AsBytes(),
			)
			_ = ifStateMap.Update(
				ifstate.NewKey(126).AsBytes(),
				ifstate.NewValue(ifstate.FlgWEP, "eth123",
					0, 13, 14, 15, 0).AsBytes(),
			)

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(ifStateMap.IsEmpty()).To(BeTrue())
			Expect(jumpMap.Contents).To(Equal(map[string]string{
				string(jump.Key(16)):                         string(jump.Value(uint32(1000 + 16))),
				string(jump.Key(16 + jump.TCMaxEntryPoints)): string(jump.Value(uint32(1000 + 16))),
			}))
			Expect(xdpJumpMap.Contents).To(Equal(map[string]string{
				// Key 4 wasn't used above so it should persist.
				string(jump.Key(4)): string(jump.Value(uint32(2000 + 4))),
			}))
		})

		dumpJumpMap := func(in *mock.Map) map[int]int {
			out := map[int]int{}
			for k, v := range in.Contents {
				parsedKey := binary.LittleEndian.Uint32([]byte(k))
				parsedVal := binary.LittleEndian.Uint32([]byte(v))
				out[int(parsedKey)] = int(parsedVal)
			}
			return out
		}

		dumpIfstateMap := func(in *mock.Map) map[int]string {
			out := map[int]string{}
			for k, v := range in.Contents {
				parsedKey := ifstate.KeyFromBytes([]byte(k))
				parsedVal := ifstate.ValueFromBytes([]byte(v))
				out[int(parsedKey.IfIndex())] = parsedVal.String()
			}
			return out
		}

		It("should reclaim indexes for active interfaces", func() {
			for i := 0; i < 8; i++ {
				_ = jumpMap.Update(jump.Key(i), jump.Value(uint32(1000+i)))
				_ = jumpMap.Update(jump.Key(i+jump.TCMaxEntryPoints), jump.Value(uint32(1000+i)))
			}
			for i := 1; i < 2; i++ {
				_ = xdpJumpMap.Update(jump.Key(i), jump.Value(uint32(2000+i)))
			}

			key123 := ifstate.NewKey(123).AsBytes()
			value123 := ifstate.NewValue(ifstate.FlgReady|ifstate.FlgWEP, "cali12345",
				-1, 0, 2, 3, 4)
			_ = ifStateMap.Update(
				key123,
				value123.AsBytes(),
			)
			_ = ifStateMap.Update(
				ifstate.NewKey(124).AsBytes(),
				ifstate.NewValue(0, "eth124",
					2, 5, 6, 7, 1).AsBytes(),
			)

			dp.interfaceByIndexFn = func(ifindex int) (*net.Interface, error) {
				if ifindex == 123 {
					return &net.Interface{
						Name:  "cali12345",
						Index: 123,
						Flags: net.FlagUp,
					}, nil
				}
				return nil, errors.New("no such network interface")
			}
			genWLUpdate("cali12345", "pol-a")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(dumpIfstateMap(ifStateMap)).To(Equal(map[int]string{
				123: value123.String(),
			}))

			// Expect clean-up deletions but no value changes due to mocking.
			Expect(dumpJumpMap(jumpMap)).To(Equal(map[int]int{
				0:     1000,
				2:     1002,
				3:     1003,
				4:     1004,
				10000: 1000,
				10002: 1002,
				10003: 1003,
				10004: 1004,
			}))
			Expect(dumpJumpMap(xdpJumpMap)).To(Equal(map[int]int{
				1: 2001,
			}))
		})

		It("should handle jump map collision: single iface", func() {
			// This test verifies that we recover if we're started with
			// bad data in the ifstate map; in particular if two policy
			// program indexes collide.

			key123 := ifstate.NewKey(123).AsBytes()

			// Oops, we accidentally wrote all zeros to the dataplane...
			value123Zeros := ifstate.NewValue(ifstate.FlgReady|ifstate.FlgWEP, "cali12345",
				0, 0, 0, 0, 0)
			_ = ifStateMap.Update(
				key123,
				value123Zeros.AsBytes(),
			)

			dp.interfaceByIndexFn = func(ifindex int) (*net.Interface, error) {
				if ifindex == 123 {
					return &net.Interface{
						Name:  "cali12345",
						Index: 123,
						Flags: net.FlagUp,
					}, nil
				}
				return nil, errors.New("no such network interface")
			}
			genWLUpdate("cali12345", "pol-a")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			// XDP gets cleaned up because it's a WEP, ingress keeps its
			// ID because it was the first; egress gets reallocated.
			value123Fixed := ifstate.NewValue(ifstate.FlgReady|ifstate.FlgWEP, "cali12345",
				-1, 0, 1, -1, -1)
			Expect(dumpIfstateMap(ifStateMap)).To(Equal(map[int]string{
				123: value123Fixed.String(),
			}))
		})

		It("should handle jump map collision: multi-iface", func() {
			// This test verifies that we recover if we're started with
			// bad data in the ifstate map; in particular if two policy
			// program indexes collide.

			// Oops, we accidentally wrote all zeros to the dataplane...
			key123 := ifstate.NewKey(123).AsBytes()
			value123Zeros := ifstate.NewValue(ifstate.FlgReady|ifstate.FlgWEP, "cali12345",
				0, 0, 0, 0, 0)
			_ = ifStateMap.Update(
				key123,
				value123Zeros.AsBytes(),
			)

			// ...twice.
			key124 := ifstate.NewKey(124).AsBytes()
			value124Zeros := ifstate.NewValue(ifstate.FlgReady|ifstate.FlgWEP, "cali56789",
				0, 0, 0, 0, 0)
			_ = ifStateMap.Update(
				key124,
				value124Zeros.AsBytes(),
			)

			dp.interfaceByIndexFn = func(ifindex int) (*net.Interface, error) {
				if ifindex == 123 {
					return &net.Interface{
						Name:  "cali12345",
						Index: 123,
						Flags: net.FlagUp,
					}, nil
				}
				if ifindex == 124 {
					return &net.Interface{
						Name:  "cali56789",
						Index: 124,
						Flags: net.FlagUp,
					}, nil
				}
				return nil, errors.New("no such network interface")
			}
			genWLUpdate("cali12345", "pol-a")()
			genWLUpdate("cali56789", "pol-b")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			// Everything collides but the allocations are non-deterministic
			// so we need to check them by hand...
			Expect(ifStateMap.Contents).To(HaveLen(2))
			val123 := ifstate.ValueFromBytes([]byte(ifStateMap.Contents[string(key123)]))
			val124 := ifstate.ValueFromBytes([]byte(ifStateMap.Contents[string(key124)]))

			tcIDsSeen := set.New[int]()
			for _, v := range []ifstate.Value{val123, val124} {
				Expect(v.XDPPolicy()).To(Equal(-1), "WEPs shouldn't get XDP IDs")

				Expect(v.IngressPolicy()).NotTo(Equal(-1), "WEPs should have ingress pol")
				Expect(tcIDsSeen.Contains(v.IngressPolicy())).To(BeFalse(), "Saw same jump map ID more than once")
				tcIDsSeen.Add(v.IngressPolicy())

				Expect(v.EgressPolicy()).NotTo(Equal(-1), "WEPs should have egress pol")
				Expect(tcIDsSeen.Contains(v.EgressPolicy())).To(BeFalse(), "Saw same jump map ID more than once")
				tcIDsSeen.Add(v.EgressPolicy())

				Expect(v.TcIngressFilter()).To(Equal(-1), "should be no filters in use")
				Expect(v.TcEgressFilter()).To(Equal(-1), "should be no filters in use")
			}
		})

		It("should handle jump map collision: multi-iface HEPs", func() {
			// Verify collision of two HEPs (using same policy IDs), they
			// use XDP too.

			// Oops, we accidentally wrote all zeros to the dataplane...
			key123 := ifstate.NewKey(123).AsBytes()
			value123Zeros := ifstate.NewValue(ifstate.FlgReady, "eth0",
				0, 0, 0, 0, -1)
			_ = ifStateMap.Update(key123, value123Zeros.AsBytes())

			// ...twice.
			key124 := ifstate.NewKey(124).AsBytes()
			// Using eth0a because the data iface pattern is eth0 (other tests
			// use eth1 for something else...).
			value124Zeros := ifstate.NewValue(ifstate.FlgReady, "eth0a",
				0, 0, 0, -1, 0)
			_ = ifStateMap.Update(key124, value124Zeros.AsBytes())

			dp.interfaceByIndexFn = func(ifindex int) (*net.Interface, error) {
				if ifindex == 123 {
					return &net.Interface{
						Name:  "eth0",
						Index: 123,
						Flags: net.FlagUp,
					}, nil
				}
				if ifindex == 124 {
					return &net.Interface{
						Name:  "eth0a",
						Index: 124,
						Flags: net.FlagUp,
					}, nil
				}
				return nil, errors.New("no such network interface")
			}

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			// Everything collides but the allocations are non-deterministic
			// so we need to check them by hand...
			ifDump := dumpIfstateMap(ifStateMap)
			Expect(ifDump).To(HaveLen(2))
			Expect(ifDump).To(HaveKey(123))
			Expect(ifDump).To(HaveKey(124))
			val123 := ifstate.ValueFromBytes([]byte(ifStateMap.Contents[string(key123)]))
			val124 := ifstate.ValueFromBytes([]byte(ifStateMap.Contents[string(key124)]))

			xdpIDsSeen := set.New[int]()
			tcIDsSeen := set.New[int]()
			for _, v := range []ifstate.Value{val123, val124} {
				Expect(v.XDPPolicy()).NotTo(Equal(-1), "WEPs shouldn't get XDP IDs")
				Expect(xdpIDsSeen.Contains(v.XDPPolicy())).To(BeFalse(), fmt.Sprintf("Saw same jump XDP map ID %d more than once", v.XDPPolicy()))
				xdpIDsSeen.Add(v.XDPPolicy())

				Expect(v.IngressPolicy()).NotTo(Equal(-1), "WEPs should have ingress pol")
				Expect(tcIDsSeen.Contains(v.IngressPolicy())).To(BeFalse(), "Saw same jump map ID more than once")
				tcIDsSeen.Add(v.IngressPolicy())

				Expect(v.EgressPolicy()).NotTo(Equal(-1), "WEPs should have egress pol")
				Expect(tcIDsSeen.Contains(v.EgressPolicy())).To(BeFalse(), "Saw same jump map ID more than once")
				tcIDsSeen.Add(v.EgressPolicy())

				Expect(v.TcIngressFilter()).To(Equal(-1), "should be no filters in use")
				Expect(v.TcEgressFilter()).To(Equal(-1), "should be no filters in use")
			}
		})

		It("should clean up with update", func() {
			_ = ifStateMap.Update(
				ifstate.NewKey(123).AsBytes(),
				ifstate.NewValue(ifstate.FlgReady, "eth123", -1, -1, -1, -1, -1).AsBytes(),
			)

			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genWLUpdate("cali12345")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(ifStateMap.ContainsKey(ifstate.NewKey(123).AsBytes())).To(BeFalse())
			checkIfState(15, "cali12345", ifstate.FlgWEP|ifstate.FlgReady)
		})

		It("iface up -> wl", func() {
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genWLUpdate("cali12345")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			checkIfState(15, "cali12345", ifstate.FlgWEP|ifstate.FlgReady)
		})

		It("iface up -> defer -> wl", func() {
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			checkIfState(15, "cali12345", ifstate.FlgWEP|ifstate.FlgReady)

			genWLUpdate("cali12345")()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			checkIfState(15, "cali12345", ifstate.FlgWEP|ifstate.FlgReady)
		})

		It("wl -> iface up", func() {
			genWLUpdate("cali12345")()
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			checkIfState(15, "cali12345", ifstate.FlgWEP|ifstate.FlgReady)
		})

		It("wl -> defer -> iface up", func() {
			genWLUpdate("cali12345")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())
			Expect(ifStateMap.ContainsKey(ifstate.NewKey(uint32(15)).AsBytes())).To(BeFalse())

			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			checkIfState(15, "cali12345", ifstate.FlgWEP|ifstate.FlgReady)
		})

		It("iface up -> wl -> iface down", func() {
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genWLUpdate("cali12345")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			genIfaceUpdate("cali12345", ifacemonitor.StateDown, 15)()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(ifStateMap.ContainsKey(ifstate.NewKey(15).AsBytes())).To(BeFalse())
		})

		It("iface up -> wl -> iface down, up, down", func() {
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genWLUpdate("cali12345")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			genIfaceUpdate("cali12345", ifacemonitor.StateDown, 15)()
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genIfaceUpdate("cali12345", ifacemonitor.StateDown, 15)()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(ifStateMap.ContainsKey(ifstate.NewKey(15).AsBytes())).To(BeFalse())
		})

		It("iface up -> wl -> iface down -> iface up", func() {
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genWLUpdate("cali12345")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			genIfaceUpdate("cali12345", ifacemonitor.StateDown, 15)()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			checkIfState(15, "cali12345", ifstate.FlgWEP|ifstate.FlgReady)
		})
	})

	Describe("program map", func() {
		JustBeforeEach(func() {
			mockDP = &mockProgMapDP{
				dp,
			}
			newBpfEpMgr()

			bpfEpMgr.bpfLogLevel = "debug"
			bpfEpMgr.logFilters = map[string]string{"all": "tcp"}
		})

		It("should clean up WL policies and log filters when iface down", func() {
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genPolicy("default", "mypolicy")()
			genWLUpdate("cali12345", "mypolicy")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(jumpMap.Contents).To(HaveLen(4)) // 2x policy and 2x filter
			Expect(xdpJumpMap.Contents).To(HaveLen(0))

			genIfaceUpdate("cali12345", ifacemonitor.StateDown, 15)()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(jumpMap.Contents).To(HaveLen(0))
			Expect(xdpJumpMap.Contents).To(HaveLen(0))
		})

		It("should clean up HEP policies and log filters when iface down", func() {
			genIfaceUpdate("eth0", ifacemonitor.StateUp, 10)()
			genUntracked("default", "untracked1")()
			genPolicy("default", "mypolicy")()
			hostEp := hostEpNorm
			hostEp.UntrackedTiers = []*proto.TierInfo{{
				Name:            "default",
				IngressPolicies: []string{"untracked1"},
			}}
			genHEPUpdate("eth0", hostEp)()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(jumpMap.Contents).To(HaveLen(4))    // 2x policy and 2x filter
			Expect(xdpJumpMap.Contents).To(HaveLen(1)) // 1x policy

			genIfaceUpdate("eth0", ifacemonitor.StateDown, 10)()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(jumpMap.Contents).To(HaveLen(0))
			Expect(xdpJumpMap.Contents).To(HaveLen(0))
		})

		It("should not update the wep log filter if only policy changes", func() {
			dp.ensureQdiscFn = func(iface string) (bool, error) {
				if iface == "cali12345" {
					return true, nil
				}
				return false, nil
			}
			genIfaceUpdate("cali12345", ifacemonitor.StateUp, 15)()
			genPolicy("default", "mypolicy")()
			genWLUpdate("cali12345", "mypolicy")()

			err := bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(jumpMap.Contents).To(HaveLen(4)) // 2x policy and 2x filter
			Expect(xdpJumpMap.Contents).To(HaveLen(0))

			jumpCopyContents := make(map[string]string, len(jumpMap.Contents))
			for k, v := range jumpMap.Contents {
				jumpCopyContents[k] = v
			}

			genPolicy("default", "anotherpolicy")()
			genWLUpdate("cali12345", "anotherpolicy")()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(len(jumpCopyContents)).To(Equal(len(jumpMap.Contents)))
			Expect(jumpCopyContents).NotTo(Equal(jumpMap.Contents))

			changes := 0

			for k, v := range jumpCopyContents {
				if v != jumpMap.Contents[k] {
					changes++
				}
			}

			Expect(changes).To(Equal(2)) // only policies have changed

			// restart

			jumpCopyContents = make(map[string]string, len(jumpMap.Contents))
			for k, v := range jumpMap.Contents {
				jumpCopyContents[k] = v
			}

			dp = newMockDataplane()
			mockDP = &mockProgMapDP{
				dp,
			}
			newBpfEpMgr()

			bpfEpMgr.bpfLogLevel = "debug"
			bpfEpMgr.logFilters = map[string]string{"all": "tcp"}

			dp.interfaceByIndexFn = func(ifindex int) (*net.Interface, error) {
				if ifindex == 15 {
					return &net.Interface{
						Index: 15,
						Name:  "cali12345",
						Flags: net.FlagUp,
					}, nil
				}
				return nil, errors.New("no such network interface")
			}
			genPolicy("default", "anotherpolicy")()
			genWLUpdate("cali12345", "anotherpolicy")()

			err = bpfEpMgr.CompleteDeferredWork()
			Expect(err).NotTo(HaveOccurred())

			Expect(len(jumpCopyContents)).To(Equal(len(jumpMap.Contents)))
			Expect(jumpCopyContents).NotTo(Equal(jumpMap.Contents))

			changes = 0

			for k, v := range jumpCopyContents {
				if v != jumpMap.Contents[k] {
					changes++
				}
			}

			// After a restart, even devices that are in ready state get both
			// programs reapplied as the configuration of logfilters coul dhave
			// changed.
			Expect(changes).To(Equal(4))
		})
	})
})

var _ = Describe("jumpMapAlloc tests", func() {
	var jma *jumpMapAlloc

	BeforeEach(func() {
		jma = newJumpMapAlloc(5)
	})

	It("should give initial values in order", func() {
		for i := 0; i < 5; i++ {
			idx, err := jma.Get("test")
			Expect(err).NotTo(HaveOccurred())
			Expect(idx).To(Equal(i))
		}
		idx, err := jma.Get("test")
		Expect(err).To(HaveOccurred())
		Expect(idx).To(Equal(-1))
	})

	It("should allow explicit assign", func() {
		// Free stack is [4,3,2,1,0] to begin with

		err := jma.Assign(0, "test")
		Expect(err).NotTo(HaveOccurred())
		// Free stack now [4,3,2,1] -> 0

		err = jma.Assign(3, "test")
		Expect(err).NotTo(HaveOccurred())
		// 3 gets swapped to end then popped:
		// Free stack now [4,1,2] -> 3

		Expect(jma.Get("test")).To(Equal(2))
		Expect(jma.Get("test")).To(Equal(1))
		Expect(jma.Get("test")).To(Equal(4))
		_, err = jma.Get("test")
		Expect(err).To(HaveOccurred())
	})

	It("should re-use items put back", func() {
		idx0, err := jma.Get("test0")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx0).To(Equal(0))
		idx1, err := jma.Get("test1")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx1).To(Equal(1))
		Expect(jma.Put(idx0, "test0")).To(Succeed())
		// Should re-use the value we put back in.
		idx2, err := jma.Get("test2")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx2).To(Equal(0))
	})

	It("should assign ownership when calling Assign", func() {
		err := jma.Assign(3, "test0")
		Expect(err).NotTo(HaveOccurred())

		Expect(jma.Put(3, "test1")).NotTo(Succeed())
		Expect(jma.Put(3, "test0")).To(Succeed())
	})

	It("should reject Put from wrong owner", func() {
		idx0, err := jma.Get("test0")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx0).To(Equal(0))
		idx1, err := jma.Get("test1")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx1).To(Equal(1))

		Expect(jma.Put(idx0, "test1")).NotTo(Succeed())

		// Should ignore the value we put back in.
		idx2, err := jma.Get("test2")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx2).To(Equal(2))
	})

	It("should reject Put if already free", func() {
		idx0, err := jma.Get("test0")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx0).To(Equal(0))

		Expect(jma.Put(idx0, "")).NotTo(Succeed())
		Expect(jma.Put(idx0, "test0")).To(Succeed())
		Expect(jma.Put(idx0, "test0")).NotTo(Succeed())
		Expect(jma.Put(idx0, "test1")).NotTo(Succeed())
	})

	It("should reject Assign if already assigned", func() {
		idx0, err := jma.Get("test0")
		Expect(err).NotTo(HaveOccurred())
		Expect(idx0).To(Equal(0))

		Expect(jma.Assign(idx0, "")).NotTo(Succeed())
		Expect(jma.Assign(idx0, "test0")).NotTo(Succeed())
		Expect(jma.Assign(idx0, "test1")).NotTo(Succeed())
	})

	It("should reject Assign out of range", func() {
		Expect(jma.Assign(-1, "test0")).NotTo(Succeed())
		Expect(jma.Assign(10, "test0")).NotTo(Succeed())
	})
})
