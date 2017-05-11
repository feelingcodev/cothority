/*
Identity is a service that allows storing of key/value pairs that belong to
a given identity that is shared between multiple devices. In order to
add/remove devices or add/remove key/value-pairs, a 'threshold' of devices
need to vote on those changes.

The key/value-pairs are stored in a personal blockchain and signed by the
cothority using forward-links, so that an external observer can check the
collective signatures and be assured that the blockchain is valid.
*/

package service

import (
	"reflect"
	"sync"

	"errors"

	"github.com/dedis/cothority/identity"
	"github.com/dedis/cothority/messaging"
	"github.com/dedis/cothority/skipchain"
	// as we use the skipchain-service, make sure it will be loaded
	_ "github.com/dedis/cothority/skipchain/service"
	"gopkg.in/dedis/onet.v1"
	"gopkg.in/dedis/onet.v1/crypto"
	"gopkg.in/dedis/onet.v1/log"
	"gopkg.in/dedis/onet.v1/network"
)

// How many msec to wait before a timeout is generated in the propagation
const propagateTimeout = 10000

var identityService onet.ServiceID

func init() {
	identityService, _ = onet.RegisterNewService(identity.ServiceName, newIdentityService)
	network.RegisterMessage(&StorageMap{})
	network.RegisterMessage(&Storage{})
}

// Service handles identities
type Service struct {
	*onet.ServiceProcessor
	*StorageMap
	propagateIdentity  messaging.PropagationFunc
	propagateSkipBlock messaging.PropagationFunc
	propagateConfig    messaging.PropagationFunc
	identitiesMutex    sync.Mutex
	skipchain          *skipchain.Client
}

// StorageMap holds the map to the storages so it can be marshaled.
type StorageMap struct {
	Identities map[string]*Storage
}

// Storage stores one identity together with the skipblocks.
type Storage struct {
	sync.Mutex
	Latest   *identity.Config
	Proposed *identity.Config
	Votes    map[string]*crypto.SchnorrSig
	Data     *skipchain.SkipBlock
}

/*
 * API messages
 */

// CreateIdentity stores a new identity for propose-config
func (s *Service) CreateIdentity(ci *identity.CreateIdentity) (*identity.CreateIdentityReply, onet.ClientError) {
	var sbData *skipchain.SkipBlock
	if ci.Roster != nil {
		var err error
		sbData, err = skipchain.NewClient().CreateGenesis(ci.Roster, 4, 4,
			identity.VerificationIdentity, ci.Config, nil)
		if err != nil {
			return nil, onet.NewClientError(err)
		}
	}
	answ, err := s.propagateIdentity(ci.Roster, &PropagateIdentity{sbData}, propagateTimeout)
	if err != nil {
		return nil, onet.NewClientErrorCode(identity.ErrorOnet, err.Error())
	}
	if answ < len(ci.Roster.List) {
		log.Warn("Did not get answer from everybody")
	}
	return &identity.CreateIdentityReply{Genesis: sbData}, nil
}

// ProposeSend only stores the proposed configuration internally. Signatures
// come later.
func (s *Service) ProposeSend(p *identity.ProposeSend) (network.Message, onet.ClientError) {
	log.Lvl2(s, "Storing new proposal")
	sid := s.getIdentityStorage(p.ID)
	if sid == nil {
		return nil, onet.NewClientErrorCode(identity.ErrorBlockMissing, "Didn't find Identity")
	}
	roster := sid.Data.Roster
	replies, err := s.propagateConfig(roster, p, propagateTimeout)
	if err != nil {
		return nil, onet.NewClientErrorCode(identity.ErrorOnet, err.Error())
	}
	if replies != len(roster.List) {
		log.Warn("Did only get", replies, "out of", len(roster.List))
	}
	return nil, nil
}

// ProposeUpdate returns an eventual config-proposition
func (s *Service) ProposeUpdate(cnc *identity.ProposeUpdate) (*identity.ProposeUpdateReply, onet.ClientError) {
	log.Lvl3(s, "Sending proposal-update to client")
	sid := s.getIdentityStorage(cnc.ID)
	if sid == nil {
		return nil, onet.NewClientErrorCode(identity.ErrorBlockMissing, "Didn't find Identity")
	}
	sid.Lock()
	defer sid.Unlock()
	return &identity.ProposeUpdateReply{
		Propose: sid.Proposed,
	}, nil
}

// ProposeVote takes int account a vote for the proposed config. It also verifies
// that the voter is in the latest config.
// An empty signature signifies that the vote has been rejected.
func (s *Service) ProposeVote(v *identity.ProposeVote) (*identity.ProposeVoteReply, onet.ClientError) {
	log.Lvl2(s, "Voting on proposal")
	// First verify if the signature is legitimate
	sid := s.getIdentityStorage(v.ID)
	if sid == nil {
		return nil, onet.NewClientErrorCode(identity.ErrorBlockMissing, "Didn't find identity")
	}

	// Putting this in a function because of the lock which needs to be held
	// over all calls that might return an error.
	cerr := func() onet.ClientError {
		sid.Lock()
		defer sid.Unlock()
		log.Lvl3("Voting on", sid.Proposed.Device)
		owner, ok := sid.Latest.Device[v.Signer]
		if !ok {
			return onet.NewClientErrorCode(identity.ErrorAccountMissing, "Didn't find signer")
		}
		if sid.Proposed == nil {
			return onet.NewClientErrorCode(identity.ErrorConfigMissing, "No proposed block")
		}
		hash, err := sid.Proposed.Hash()
		if err != nil {
			return onet.NewClientErrorCode(identity.ErrorOnet, "Couldn't get hash")
		}
		if _, exists := sid.Votes[v.Signer]; exists {
			return onet.NewClientErrorCode(identity.ErrorVoteDouble, "Already voted for that block")
		}
		log.Lvl3(v.Signer, "voted", v.Signature)
		if v.Signature != nil {
			err = crypto.VerifySchnorr(network.Suite, owner.Point, hash, *v.Signature)
			if err != nil {
				return onet.NewClientErrorCode(identity.ErrorVoteSignature, "Wrong signature: "+err.Error())
			}
		}
		return nil
	}()
	if cerr != nil {
		return nil, cerr
	}

	// Propagate the vote
	_, err := s.propagateConfig(sid.Data.Roster, v, propagateTimeout)
	if err != nil {
		return nil, onet.NewClientErrorCode(identity.ErrorOnet, cerr.Error())
	}
	if len(sid.Votes) >= sid.Latest.Threshold ||
		len(sid.Votes) == len(sid.Latest.Device) {
		// If we have enough signatures, make a new data-skipblock and
		// propagate it
		log.Lvl3("Having majority or all votes")

		// Making a new data-skipblock
		log.Lvl3("Sending data-block with", sid.Proposed.Device)
		reply, cerr := s.skipchain.AddSkipBlock(sid.Data, nil, sid.Proposed)
		if cerr != nil {
			return nil, cerr
		}
		_, msg, _ := network.Unmarshal(reply.Latest.Data)
		log.Lvl3("SB signed is", msg.(*identity.Config).Device)
		usb := &UpdateSkipBlock{
			ID:     v.ID,
			Latest: reply.Latest,
		}
		_, err = s.propagateSkipBlock(sid.Data.Roster, usb, propagateTimeout)
		if err != nil {
			return nil, onet.NewClientErrorCode(identity.ErrorOnet, cerr.Error())
		}
		return &identity.ProposeVoteReply{Data: sid.Data}, nil
	}
	return nil, nil
}

/*
 * Internal messages
 */

// propagateConfig handles propagation of all configuration-proposals in the identity-service.
func (s *Service) propagateConfigHandler(msg network.Message) {
	log.Lvlf4("Got msg %+v %v", msg, reflect.TypeOf(msg).String())
	id := skipchain.SkipBlockID(nil)
	switch msg.(type) {
	case *identity.ProposeSend:
		id = msg.(*identity.ProposeSend).ID
	case *identity.ProposeVote:
		id = msg.(*identity.ProposeVote).ID
	default:
		log.Errorf("Got an unidentified propagation-request: %v", msg)
		return
	}

	if id != nil {
		sid := s.getIdentityStorage(id)
		if sid == nil {
			log.Error("Didn't find entity in", s)
			return
		}
		sid.Lock()
		defer sid.Unlock()
		switch msg.(type) {
		case *identity.ProposeSend:
			p := msg.(*identity.ProposeSend)
			sid.Proposed = p.Propose
			sid.Votes = make(map[string]*crypto.SchnorrSig)
		case *identity.ProposeVote:
			v := msg.(*identity.ProposeVote)
			sid.Votes[v.Signer] = v.Signature
		}
		s.save()
	}
}

// propagateSkipBlock saves a new skipblock to the identity
func (s *Service) propagateSkipBlockHandler(msg network.Message) {
	log.Lvlf4("Got msg %+v %v", msg, reflect.TypeOf(msg).String())
	usb, ok := msg.(*UpdateSkipBlock)
	if !ok {
		log.Error("Wrong message-type")
		return
	}
	sid := s.getIdentityStorage(usb.ID)
	if sid == nil {
		log.Error("Didn't find entity in", s)
		return
	}
	sid.Lock()
	defer sid.Unlock()
	skipblock := msg.(*UpdateSkipBlock).Latest
	_, msgLatest, err := network.Unmarshal(skipblock.Data)
	if err != nil {
		log.Error(err)
		return
	}
	al, ok := msgLatest.(*identity.Config)
	if !ok {
		log.Error(err)
		return
	}
	sid.Data = skipblock
	sid.Latest = al
	sid.Proposed = nil
	s.save()
}

// propagateIdentity stores a new identity in all nodes.
func (s *Service) propagateIdentityHandler(msg network.Message) {
	log.Lvlf4("Got msg %+v %v", msg, reflect.TypeOf(msg).String())
	pi, ok := msg.(*PropagateIdentity)
	if !ok {
		log.Error("Got a wrong message for propagation")
		return
	}
	id := skipchain.SkipBlockID(pi.NewBlock.Hash)
	if s.getIdentityStorage(id) != nil {
		log.Errorf("Identity %x already exists", id)
		return
	}
	log.Lvl3("Storing identity in", s)
	_, cfg, err := network.Unmarshal(pi.NewBlock.Data)
	if err != nil {
		log.Error("Couldn't get config-data")
		return
	}
	storage := &Storage{
		Latest: cfg.(*identity.Config),
		Votes:  make(map[string]*crypto.SchnorrSig),
		Data:   pi.NewBlock,
	}
	s.setIdentityStorage(id, storage)
	return
}

// getIdentityStorage returns the corresponding IdentityStorage or nil
// if none was found
func (s *Service) getIdentityStorage(id skipchain.SkipBlockID) *Storage {
	s.identitiesMutex.Lock()
	defer s.identitiesMutex.Unlock()
	is, ok := s.Identities[string(id)]
	if !ok {
		return nil
	}
	return is
}

// setIdentityStorage saves an IdentityStorage
func (s *Service) setIdentityStorage(id skipchain.SkipBlockID, is *Storage) {
	s.identitiesMutex.Lock()
	defer s.identitiesMutex.Unlock()
	log.Lvlf3("%s %x %v", s.Context.ServerIdentity(), id[0:8], is.Latest.Device)
	s.Identities[string(id)] = is
	s.save()
}

// saves the actual identity
func (s *Service) save() {
	log.Lvl3("Saving service")
	err := s.Save("storage", s.StorageMap)
	if err != nil {
		log.Error("Couldn't save file:", err)
	}
}

func (s *Service) clearIdentities() {
	s.Identities = make(map[string]*Storage)
}

// Tries to load the configuration and updates if a configuration
// is found, else it returns an error.
func (s *Service) tryLoad() error {
	if !s.DataAvailable("storage") {
		return nil
	}
	msg, err := s.Load("storage")
	if err != nil {
		return err
	}
	var ok bool
	s.StorageMap, ok = msg.(*StorageMap)
	if !ok {
		return errors.New("Data of wrong type")
	}
	log.Lvl3("Successfully loaded")
	return nil
}

func (s *Service) verifyNewBlock(sb *skipchain.SkipBlock) bool {
	return true
}

func newIdentityService(c *onet.Context) onet.Service {
	s := &Service{
		ServiceProcessor: onet.NewServiceProcessor(c),
		StorageMap:       &StorageMap{make(map[string]*Storage)},
		skipchain:        skipchain.NewClient(),
	}
	var err error
	s.propagateIdentity, err =
		messaging.NewPropagationFunc(c, "IdentityPropagateID", s.propagateIdentityHandler)
	if err != nil {
		return nil
	}
	s.propagateSkipBlock, err =
		messaging.NewPropagationFunc(c, "IdentityPropagateSB", s.propagateSkipBlockHandler)
	if err != nil {
		return nil
	}
	s.propagateConfig, err =
		messaging.NewPropagationFunc(c, "IdentityPropagateConf", s.propagateConfigHandler)
	if err != nil {
		return nil
	}
	if err := s.tryLoad(); err != nil {
		log.Error(err)
	}
	service.RegisterVerification(c, identity.VerifyIdentity, s.verifyNewBlock)
	log.ErrFatal(s.RegisterHandlers(s.ProposeSend, s.ProposeVote,
		s.ProposeUpdate, s.CreateIdentity))
	return s
}
