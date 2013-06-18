package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go.crypto/curve25519"
	"code.google.com/p/go.crypto/nacl/box"
	"code.google.com/p/go.net/proxy"
	"code.google.com/p/goprotobuf/proto"
	"github.com/agl/ed25519"
	"github.com/agl/pond/bbssig"
	pond "github.com/agl/pond/protos"
	"github.com/agl/pond/transport"
)

const (
	nonceLen          = 24
	ephemeralBlockLen = nonceLen + 32 + box.Overhead
)

func (c *client) send(to *Contact, message *pond.Message) error {
	messageBytes, err := proto.Marshal(message)
	if err != nil {
		return err
	}

	if len(messageBytes) > pond.MaxSerializedMessage {
		return errors.New("message too large")
	}

	plaintext := make([]byte, pond.MaxSerializedMessage+4)
	binary.LittleEndian.PutUint32(plaintext, uint32(len(messageBytes)))
	copy(plaintext[4:], messageBytes)
	c.randBytes(plaintext[4+len(messageBytes):])

	var innerNonce [24]byte
	c.randBytes(innerNonce[:])
	var sealed, innerSealed []byte
	sealedLen := nonceLen + len(plaintext) + box.Overhead
	dhPrivate := &to.lastDHPrivate

	if to.supportedVersion >= 1 {
		public, private, err := box.GenerateKey(c.rand)
		if err != nil {
			return err
		}
		dhPrivate = private

		var outerNonce [24]byte
		c.randBytes(outerNonce[:])
		sealedLen += ephemeralBlockLen
		sealed = make([]byte, sealedLen)
		copy(sealed, outerNonce[:])
		box.Seal(sealed[nonceLen:nonceLen], public[:], &outerNonce, &to.theirCurrentDHPublic, &to.lastDHPrivate)
		innerSealed = sealed[ephemeralBlockLen:]
	} else {
		sealed = make([]byte, sealedLen)
		innerSealed = sealed
	}

	copy(innerSealed, innerNonce[:])
	box.Seal(innerSealed[nonceLen:nonceLen], plaintext, &innerNonce, &to.theirCurrentDHPublic, dhPrivate)

	sha := sha256.New()
	sha.Write(sealed)
	digest := sha.Sum(nil)
	sha.Reset()
	groupSig, err := to.myGroupKey.Sign(c.rand, digest, sha)
	if err != nil {
		return err
	}

	request := &pond.Request{
		Deliver: &pond.Delivery{
			To:         to.theirIdentityPublic[:],
			Signature:  groupSig,
			Generation: proto.Uint32(to.generation),
			Message:    sealed,
		},
	}
	out := &queuedMessage{
		request: request,
		id:      *message.Id,
		to:      to.id,
		server:  to.theirServer,
		message: message,
		created: time.Unix(*message.Time, 0),
	}
	c.enqueue(out)
	if len(message.Body) > 0 {
		c.outboxUI.Add(*message.Id, to.name, out.created.Format(shortTimeFormat), indicatorRed)
	}
	c.outbox = append(c.outbox, out)

	return nil
}

// revocationSignaturePrefix is prepended to a SignedRevocation_Revocation
// message before signing in order to give context to the signature.
var revocationSignaturePrefix = []byte("revocation\x00")

func (c *client) revoke(to *Contact) {
	to.revoked = true
	revocation := c.groupPriv.GenerateRevocation(to.groupKey)
	now := time.Now()

	groupCopy, _ := new(bbssig.Group).Unmarshal(c.groupPriv.Group.Marshal())
	groupPrivCopy, _ := new(bbssig.PrivateKey).Unmarshal(groupCopy, c.groupPriv.Marshal())
	c.prevGroupPrivs = append(c.prevGroupPrivs, previousGroupPrivateKey{
		priv:    groupPrivCopy,
		expired: now,
	})

	for _, contact := range c.contacts {
		if contact == to {
			continue
		}
		contact.previousTags = append(contact.previousTags, previousTag{
			tag:     contact.groupKey.Tag(),
			expired: now,
		})
		contact.groupKey.Update(revocation)
	}

	rev := &pond.SignedRevocation_Revocation{
		Revocation: revocation.Marshal(),
		Generation: proto.Uint32(c.generation),
	}

	c.groupPriv.Group.Update(revocation)
	c.generation++

	revBytes, err := proto.Marshal(rev)
	if err != nil {
		panic(err)
	}

	var signed []byte
	signed = append(signed, revocationSignaturePrefix...)
	signed = append(signed, revBytes...)

	sig := ed25519.Sign(&c.priv, signed)

	signedRev := pond.SignedRevocation{
		Revocation: rev,
		Signature:  sig[:],
	}

	request := &pond.Request{
		Revocation: &signedRev,
	}

	out := &queuedMessage{
		revocation: true,
		request:    request,
		id:         c.randId(),
		to:         to.id,
		server:     to.theirServer,
		created:    time.Now(),
	}
	c.enqueue(out)
	c.outboxUI.Add(out.id, "Revocation", out.created.Format(shortTimeFormat), indicatorRed)
	c.outboxUI.SetInsensitive(out.id)
	c.outbox = append(c.outbox, out)
}

func decryptMessage(sealed []byte, nonce *[24]byte, from *Contact) ([]byte, bool) {
	plaintext, ok := decryptMessageInner(sealed, nonce, from)
	if ok {
		return plaintext, true
	}

	// The message might have an ephemeral block, the nonce of which has already been split off.
	headerLen := ephemeralBlockLen - len(nonce)
	if len(sealed) > headerLen {
		publicBytes, ok := decryptMessageInner(sealed[:headerLen], nonce, from)
		if !ok || len(publicBytes) != 32 {
			return nil, false
		}
		var innerNonce [nonceLen]byte
		sealed = sealed[headerLen:]
		copy(innerNonce[:], sealed)
		sealed = sealed[nonceLen:]
		var ephemeralPublicKey [32]byte
		copy(ephemeralPublicKey[:], publicBytes)

		if plaintext, ok := box.Open(nil, sealed, &innerNonce, &ephemeralPublicKey, &from.lastDHPrivate); ok {
			return plaintext, ok
		}

		plaintext, ok := box.Open(nil, sealed, &innerNonce, &ephemeralPublicKey, &from.currentDHPrivate)
		if !ok {
			return nil, false
		}
		// They have clearly received our current DH value. Time to
		// rotate.
		copy(from.lastDHPrivate[:], from.currentDHPrivate[:])
		if _, err := io.ReadFull(rand.Reader, from.currentDHPrivate[:]); err != nil {
			panic(err)
		}
		return plaintext, true
	}

	return nil, false
}

func decryptMessageInner(sealed []byte, nonce *[24]byte, from *Contact) ([]byte, bool) {
	if plaintext, ok := box.Open(nil, sealed, nonce, &from.theirLastDHPublic, &from.lastDHPrivate); ok {
		return plaintext, true
	}

	if plaintext, ok := box.Open(nil, sealed, nonce, &from.theirCurrentDHPublic, &from.lastDHPrivate); ok {
		return plaintext, true
	}

	plaintext, ok := box.Open(nil, sealed, nonce, &from.theirLastDHPublic, &from.currentDHPrivate)
	if !ok {
		plaintext, ok = box.Open(nil, sealed, nonce, &from.theirCurrentDHPublic, &from.currentDHPrivate)
		if !ok {
			return nil, false
		}
	}

	// They have clearly received our current DH value. Time to
	// rotate.
	copy(from.lastDHPrivate[:], from.currentDHPrivate[:])
	if _, err := io.ReadFull(rand.Reader, from.currentDHPrivate[:]); err != nil {
		panic(err)
	}
	return plaintext, true
}

func (c *client) processNewMessage(m NewMessage) {
	defer func() { m.ack <- true }()

	if m.fetched != nil {
		c.processFetch(m)
	} else {
		c.processServerAnnounce(m)
	}
}

func (c *client) processFetch(m NewMessage) {
	f := m.fetched

	sha := sha256.New()
	sha.Write(f.Message)
	digest := sha.Sum(nil)

	var tag []byte
	var ok bool
	if c.groupPriv.Verify(digest, sha, f.Signature) {
		tag, ok = c.groupPriv.Open(f.Signature)
	} else {
		found := false
		for _, prev := range c.prevGroupPrivs {
			if prev.priv.Verify(digest, sha, f.Signature) {
				found = true
				tag, ok = c.groupPriv.Open(f.Signature)
				break
			}
		}
		if !found {
			c.log.Errorf("Received message with bad group signature!")
			return
		}
	}
	if !ok {
		c.log.Errorf("Failed to open group signature")
		return
	}

	var from *Contact
NextCandidate:
	for _, candidate := range c.contacts {
		if bytes.Equal(tag, candidate.groupKey.Tag()) {
			from = candidate
			break
		}
		for _, prevTag := range candidate.previousTags {
			if bytes.Equal(tag, prevTag.tag) {
				from = candidate
				break NextCandidate
			}
		}
	}

	if from == nil {
		c.log.Errorf("Message from unknown contact. Dropping. Tag: %x", tag)
		return
	}

	if from.revoked {
		// It's possible that there were pending messages from the
		// contact when we revoked them.
		c.log.Errorf("Message from revoked contact %s. Dropping", from.name)
		return
	}

	if len(f.Message) < box.Overhead+24 {
		c.log.Errorf("Message too small to process from %s", from.name)
		return
	}

	inboxMsg := &InboxMessage{
		id:           c.randId(),
		receivedTime: time.Now(),
		from:         from.id,
		sealed:       f.Message,
	}

	if !from.isPending {
		if !c.unsealMessage(inboxMsg, from) {
			return
		}
		if len(inboxMsg.message.Body) > 0 {
			subline := time.Unix(*inboxMsg.message.Time, 0).Format(shortTimeFormat)
			c.inboxUI.Add(inboxMsg.id, from.name, subline, indicatorBlue)
		}
	} else {
		c.inboxUI.Add(inboxMsg.id, from.name, "pending", indicatorRed)
	}

	c.inbox = append(c.inbox, inboxMsg)
	c.updateWindowTitle()
	c.save()
}

func (c *client) processServerAnnounce(m NewMessage) {
	inboxMsg := &InboxMessage{
		id:           c.randId(),
		receivedTime: time.Now(),
		from:         0,
		message:      m.announce.Message,
	}

	subline := time.Unix(*inboxMsg.message.Time, 0).Format(shortTimeFormat)
	c.inboxUI.Add(inboxMsg.id, "Home Server", subline, indicatorBlue)

	c.inbox = append(c.inbox, inboxMsg)
	c.updateWindowTitle()
	c.save()
}

func (c *client) unsealMessage(inboxMsg *InboxMessage, from *Contact) bool {
	if from.isPending {
		panic("was asked to unseal message from pending contact")
	}

	sealed := inboxMsg.sealed
	var nonce [24]byte
	copy(nonce[:], sealed)
	sealed = sealed[24:]
	plaintext, ok := decryptMessage(sealed, &nonce, from)

	if !ok {
		c.log.Errorf("Failed to decrypt message from %s", from.name)
		return false
	}

	if len(plaintext) < 4 {
		c.log.Errorf("Plaintext too small to process from %s", from.name)
		return false
	}

	mLen := int(binary.LittleEndian.Uint32(plaintext[:4]))
	plaintext = plaintext[4:]
	if mLen < 0 || mLen > len(plaintext) {
		c.log.Errorf("Plaintext length incorrect from %s: %d", from.name, mLen)
		return false
	}
	plaintext = plaintext[:mLen]

	msg := new(pond.Message)
	if err := proto.Unmarshal(plaintext, msg); err != nil {
		c.log.Errorf("Failed to parse mesage from %s: %s", from, err)
		return false
	}

	if l := len(msg.MyNextDh); l != len(from.theirCurrentDHPublic) {
		c.log.Errorf("Message from %s with bad DH length %d", from, l)
		return false
	}

	// Check for duplicate message.
	for _, candidate := range c.inbox {
		if candidate.from == from.id &&
			candidate.id != inboxMsg.id &&
			candidate.message != nil &&
			*candidate.message.Id == *msg.Id {
			c.log.Printf("Dropping duplicate message from %s", from.name)
			return false
		}
	}

	if !bytes.Equal(from.theirCurrentDHPublic[:], msg.MyNextDh) {
		// We have a new DH value from them.
		copy(from.theirLastDHPublic[:], from.theirCurrentDHPublic[:])
		copy(from.theirCurrentDHPublic[:], msg.MyNextDh)
	}

	if msg.InReplyTo != nil {
		id := *msg.InReplyTo

		for _, candidate := range c.outbox {
			if candidate.id == id {
				candidate.acked = time.Now()
				c.outboxUI.SetIndicator(id, indicatorGreen)
			}
		}
	}

	if msg.SupportedVersion != nil {
		from.supportedVersion = *msg.SupportedVersion
	}

	from.kxsBytes = nil
	inboxMsg.message = msg
	inboxMsg.sealed = nil
	inboxMsg.read = false

	return true
}

func (c *client) processMessageSent(msr messageSendResult) {
	var msg *queuedMessage
	for _, m := range c.outbox {
		if m.id == msr.id {
			msg = m
			break
		}
	}

	if msr.revocation != nil {
		// We tried to deliver a message to a user but the server told
		// us that there's a pending revocation.
		to := c.contacts[msg.to]

		if gen := *msr.revocation.Revocation.Generation; gen != to.generation {
			c.log.Printf("Message to '%s' resulted in revocation for generation %d, but current generation is %d", to.name, gen, to.generation)
			return
		}

		// Check the signature on the revocation.
		revBytes, err := proto.Marshal(msr.revocation.Revocation)
		if err != nil {
			c.log.Printf("Failed to marshal revocation message: %s", err)
			return
		}

		var sig [ed25519.SignatureSize]byte
		if revSig := msr.revocation.Signature; copy(sig[:], revSig) != len(sig) {
			c.log.Printf("Bad signature length on revocation (%d bytes) from %s", len(revSig), to.name)
			return
		}

		var signed []byte
		signed = append(signed, revocationSignaturePrefix...)
		signed = append(signed, revBytes...)
		if !ed25519.Verify(&to.theirPub, signed, &sig) {
			c.log.Printf("Bad signature on revocation from %s", to.name)
			return
		}
		rev, ok := new(bbssig.Revocation).Unmarshal(msr.revocation.Revocation.Revocation)
		if !ok {
			c.log.Printf("Failed to parse revocation from %s", to.name)
			return
		}
		to.generation++
		if !to.myGroupKey.Update(rev) {
			// We were revoked.
			to.revokedUs = true
			c.log.Printf("Revoked by %s", to.name)
			c.contactsUI.SetIndicator(to.id, indicatorBlack)
			c.contactsUI.SetSubline(to.id, "has revoked")

			// Mark all pending messages to this contact as
			// undeliverable.
			newQueue := make([]*queuedMessage, 0, len(c.queue))
			c.queueMutex.Lock()
			for _, m := range c.queue {
				if m.to == msg.to {
					c.outboxUI.SetIndicator(m.id, indicatorBlack)
				} else {
					newQueue = append(newQueue, m)
				}
			}
			c.queue = newQueue
			c.queueMutex.Unlock()
		} else {
			to.myGroupKey.Group.Update(rev)
			// We need to update all pending messages to this
			// contact with a new group signature. However, we
			// can't mutate entries in c.queue here because the
			// trasact goroutine is running concurrently.
			dupKey, _ := new(bbssig.MemberKey).Unmarshal(to.myGroupKey.Group, to.myGroupKey.Marshal())
			c.revocationUpdateChan <- revocationUpdate{msg.to, dupKey, to.generation}
		}
		c.ui.Actions() <- UIState{uiStateRevocationProcessed}
		c.ui.Signal()
		return
	}

	msg.sent = time.Now()
	if msg.revocation {
		c.outboxUI.SetIndicator(msg.id, indicatorGreen)
	} else {
		c.outboxUI.SetIndicator(msg.id, indicatorYellow)
	}
	c.save()
}

func decodeBase32(s string) ([]byte, error) {
	for len(s)%8 != 0 {
		s += "="
	}
	return base32.StdEncoding.DecodeString(s)
}

func replyToError(reply *pond.Reply) error {
	if reply.Status == nil || *reply.Status == pond.Reply_OK {
		return nil
	}
	if msg, ok := pond.Reply_Status_name[int32(*reply.Status)]; ok {
		return errors.New("error from server: " + msg)
	}
	return errors.New("unknown error from server: " + strconv.Itoa(int(*reply.Status)))
}

func parseServer(server string, testing bool) (serverIdentity *[32]byte, host string, err error) {
	url, err := url.Parse(server)
	if err != nil {
		return
	}
	if url.Scheme != "pondserver" {
		err = errors.New("bad URL scheme, should be pondserver")
		return
	}
	if url.User == nil || len(url.User.Username()) == 0 {
		err = errors.New("no server ID in URL")
		return
	}
	serverIdSlice, err := decodeBase32(url.User.Username())
	if err != nil {
		return
	}
	if len(serverIdSlice) != 32 {
		err = errors.New("bad server ID length")
		return
	}

	host = url.Host
	if !testing {
		if strings.ContainsRune(host, ':') {
			err = errors.New("URL contains a port number")
			return
		}
		if !strings.HasSuffix(host, ".onion") && host != "localhost" {
			err = errors.New("host is neither a .onion address nor localhost")
			return
		}
		host += ":16333"
	}

	serverIdentity = new([32]byte)
	copy(serverIdentity[:], serverIdSlice)
	return
}

// torAddr is the address at which we expect to find the local Tor SOCKS proxy.
const torAddr = "127.0.0.1:9050"

func (c *client) torDialer() proxy.Dialer {
	// We generate a random username so that Tor will decouple all of our
	// connections.
	var userBytes [8]byte
	c.randBytes(userBytes[:])
	auth := proxy.Auth{
		User:     base32.StdEncoding.EncodeToString(userBytes[:]),
		Password: "password",
	}
	dialer, err := proxy.SOCKS5("tcp", torAddr, &auth, proxy.Direct)
	if err != nil {
		panic(err)
	}
	return dialer
}

func (c *client) dialServer(server string, useRandomIdentity bool) (*transport.Conn, error) {
	identity := &c.identity
	identityPublic := &c.identityPublic
	if useRandomIdentity {
		var randomIdentity [32]byte
		c.randBytes(randomIdentity[:])

		var randomIdentityPublic [32]byte
		curve25519.ScalarBaseMult(&randomIdentityPublic, &randomIdentity)

		identity = &randomIdentity
		identityPublic = &randomIdentityPublic
	}

	serverIdentity, host, err := parseServer(server, c.testing)
	if err != nil {
		return nil, err
	}
	var tor proxy.Dialer
	if c.testing {
		tor = proxy.Direct
	} else {
		tor = c.torDialer()
	}
	rawConn, err := tor.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	// Sometimes Tor holds the connection open but we never receive
	// anything so we add a 60 second deadline.
	rawConn.SetDeadline(time.Now().Add(60 * time.Second))
	conn := transport.NewClient(rawConn, identity, identityPublic, serverIdentity)
	if err := conn.Handshake(); err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *client) doCreateAccount() error {
	_, _, err := parseServer(c.server, c.testing)
	if err != nil {
		return err
	}

	if !c.testing {
		// Check that Tor is running.
		testConn, err := net.Dial("tcp", torAddr)
		if err != nil {
			return errors.New("Failed to connect to local Tor: " + err.Error())
		}
		testConn.Close()
	}

	c.ui.Actions() <- SetText{name: "status", text: "Generating keys..."}
	c.ui.Signal()

	c.randBytes(c.identity[:])
	curve25519.ScalarBaseMult(&c.identityPublic, &c.identity)

	c.ui.Actions() <- SetText{name: "status", text: "Connecting..."}
	c.ui.Signal()

	conn, err := c.dialServer(c.server, false)
	if err != nil {
		return err
	}
	defer conn.Close()

	c.ui.Actions() <- SetText{name: "status", text: "Requesting new account..."}
	c.ui.Signal()

	c.generation = uint32(c.randId())

	request := new(pond.Request)
	request.NewAccount = &pond.NewAccount{
		Generation: proto.Uint32(c.generation),
		Group:      c.groupPriv.Group.Marshal(),
	}
	if err := conn.WriteProto(request); err != nil {
		return err
	}

	reply := new(pond.Reply)
	if err := conn.ReadProto(reply); err != nil {
		return err
	}
	if err := replyToError(reply); err != nil {
		return err
	}

	c.ui.Actions() <- SetText{name: "status", text: "Done"}
	c.ui.Signal()

	return nil
}

// resignQueuedMessages runs on the network goroutine and resigns all queued
// messages to the given contact id.
func (c *client) resignQueuedMessages(revUpdate revocationUpdate) {
	sha := sha256.New()
	var digest []byte

	for _, m := range c.queue {
		if m.to != revUpdate.id {
			continue
		}

		sha.Write(m.request.Deliver.Message)
		digest = sha.Sum(digest[:0])
		sha.Reset()
		groupSig, err := revUpdate.key.Sign(c.rand, digest, sha)
		if err != nil {
			c.log.Printf("Error while resigning after revocation: %s", err)
		}
		sha.Reset()

		m.request.Deliver.Signature = groupSig
		m.request.Deliver.Generation = proto.Uint32(revUpdate.generation)
	}
}

// transactionRateSeconds is the mean of the exponential distribution that
// we'll sample in order to distribute the time between our network
// connections.
const transactionRateSeconds = 300 // five minutes

func (c *client) transact() {
	startup := true

	var ackChan chan bool
	for {
		if !startup || !c.autoFetch {
			if ackChan != nil {
				ackChan <- true
				ackChan = nil
			}

			var timerChan <-chan time.Time
			if c.autoFetch {
				var seedBytes [8]byte
				c.randBytes(seedBytes[:])
				seed := int64(binary.LittleEndian.Uint64(seedBytes[:]))
				r := mrand.New(mrand.NewSource(seed))
				delay := r.ExpFloat64() * transactionRateSeconds
				if c.testing {
					delay = 5
				}
				c.log.Printf("Next network transaction in %d seconds", int(delay))
				timerChan = time.After(time.Duration(delay*1000) * time.Millisecond)
			}

			// Revocation updates are always processed first.
		NextEvent:
			for {
				select {
				case revUpdate, ok := <-c.revocationUpdateChan:
					if !ok {
						return
					}
					// This signals that the contact with the given
					// id has had their group signature key updated
					// and all messages in c.queue to that contact
					// need to be resigned.
					c.resignQueuedMessages(revUpdate)
					continue NextEvent
				default:
					break
				}

				var ok bool
				select {
				case ackChan, ok = <-c.fetchNowChan:
					if !ok {
						return
					}
					c.log.Printf("Starting fetch because of fetchNow signal")
					break NextEvent
				case <-timerChan:
					c.log.Printf("Starting fetch because of timer")
					break NextEvent
				case revUpdate, ok := <-c.revocationUpdateChan:
					if !ok {
						return
					}
					// This signals that the contact with the given
					// id has had their group signature key updated
					// and all messages in c.queue to that contact
					// need to be resigned.
					c.resignQueuedMessages(revUpdate)
					continue NextEvent
				}
			}
		}
		startup = false

		var head *queuedMessage
		var req *pond.Request
		var server string

		useAnonymousIdentity := true
		isFetch := false
		c.queueMutex.Lock()
		if len(c.queue) == 0 {
			useAnonymousIdentity = false
			isFetch = true
			req = &pond.Request{Fetch: &pond.Fetch{}}
			server = c.server
			c.log.Printf("Starting fetch from home server")
		} else {
			// We move the head to the back of the queue so that we
			// don't get stuck trying to send the same message over
			// and over.
			head = c.queue[0]
			c.queue = append(c.queue[1:], head)
			req = head.request
			server = head.server
			c.log.Printf("Starting message transmission to %s", server)

			if head.revocation {
				useAnonymousIdentity = false
			}
		}
		c.queueMutex.Unlock()

		conn, err := c.dialServer(server, useAnonymousIdentity)
		if err != nil {
			c.log.Printf("Failed to connect to %s: %s", server, err)
			continue
		}
		if err := conn.WriteProto(req); err != nil {
			c.log.Printf("Failed to send to %s: %s", server, err)
			continue
		}

		reply := new(pond.Reply)
		if err := conn.ReadProto(reply); err != nil {
			c.log.Printf("Failed to read from %s: %s", server, err)
			continue
		}

		if reply.Status == nil {
			if isFetch && (reply.Fetched != nil || reply.Announce != nil) {
				ackChan := make(chan bool)
				c.newMessageChan <- NewMessage{reply.Fetched, reply.Announce, ackChan}
				<-ackChan
			} else if !isFetch {
				c.queueMutex.Lock()
				c.queue = c.queue[:len(c.queue)-1]
				if len(c.queue) == 0 {
					c.queue = nil
				}
				c.queueMutex.Unlock()
				c.messageSentChan <- messageSendResult{id: head.id}
			}
		} else if !isFetch &&
			*reply.Status == pond.Reply_GENERATION_REVOKED &&
			reply.Revocation != nil {
			c.messageSentChan <- messageSendResult{id: head.id, revocation: reply.Revocation}
		}

		conn.Close()

		if err := replyToError(reply); err != nil {
			c.log.Errorf("Error from server %s: %s", server, err)
			continue
		}

	}
}

type detachmentTransfer interface {
	Request() *pond.Request
	ProcessReply(*pond.Reply) (*os.File, bool, int64, bool, error)
	Complete(conn *transport.Conn) bool
}

type uploadTransfer struct {
	id    uint64
	file  *os.File
	total int64
}

func (ut uploadTransfer) Request() *pond.Request {
	return &pond.Request{
		Upload: &pond.Upload{
			Id:   proto.Uint64(ut.id),
			Size: proto.Int64(ut.total),
		},
	}
}

func (ut uploadTransfer) ProcessReply(reply *pond.Reply) (file *os.File, isUpload bool, total int64, isComplete bool, err error) {
	var offset int64
	if reply.Upload != nil && reply.Upload.Resume != nil {
		offset = *reply.Upload.Resume
	}

	if offset == ut.total {
		isComplete = true
		return
	}
	pos, err := ut.file.Seek(offset, 0 /* from start */)
	if err != nil || pos != offset {
		err = fmt.Errorf("failed to seek in temp file: %d %d %s", pos, offset, err)
		return
	}

	file = ut.file
	isUpload = true
	total = ut.total - offset
	return
}

func (ut uploadTransfer) Complete(conn *transport.Conn) bool {
	// The server will send us a zero byte if it got everything.
	buf := []byte{1}
	io.ReadFull(conn, buf)
	return buf[0] == 0
}

func (c *client) uploadDetachment(out chan interface{}, in *os.File, id uint64, killChan chan bool) error {
	transfer := uploadTransfer{file: in, id: id}

	fi, err := in.Stat()
	if err != nil {
		return err
	}
	transfer.total = fi.Size()

	return c.transferDetachment(out, c.server, transfer, id, killChan)
}

type downloadTransfer struct {
	fileID uint64
	file   *os.File
	resume int64
	from   *[32]byte
}

func (dt downloadTransfer) Request() *pond.Request {
	var resume *int64
	if dt.resume > 0 {
		resume = proto.Int64(dt.resume)
	}

	return &pond.Request{
		Download: &pond.Download{
			From:   dt.from[:],
			Id:     proto.Uint64(dt.fileID),
			Resume: resume,
		},
	}
}

func (dt downloadTransfer) ProcessReply(reply *pond.Reply) (file *os.File, isUpload bool, total int64, isComplete bool, err error) {
	if reply.Download == nil {
		err = errors.New("Reply from server didn't include a download section")
		return
	}

	size := *reply.Download.Size
	if size < dt.resume {
		err = errors.New("Reply from server suggested that the file was truncated")
		return
	}

	file = dt.file
	total = size - dt.resume
	return
}

func (dt downloadTransfer) Complete(conn *transport.Conn) bool {
	return true
}

func (c *client) downloadDetachment(out chan interface{}, file *os.File, id uint64, downloadURL string, killChan chan bool) error {
	c.log.Printf("Starting download of %s", downloadURL)
	u, err := url.Parse(downloadURL)
	if err != nil {
		return errors.New("failed to parse download URL: " + err.Error())
	}
	if u.Scheme != "pondserver" {
		return errors.New("download URL is a not a Pond URL")
	}
	path := u.Path
	if len(path) == 0 {
		return errors.New("download URL is missing a path")
	}
	path = path[1:]
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return errors.New("download URL has incorrect number of path elements")
	}
	fromSlice, err := hex.DecodeString(parts[0])
	if err != nil {
		return errors.New("failed to parse public identity from download URL: " + err.Error())
	}
	if len(fromSlice) != 32 {
		return errors.New("public identity in download URL is wrong length")
	}
	var from [32]byte
	copy(from[:], fromSlice)

	fileID, err := strconv.ParseUint(parts[1], 16, 64)
	if err != nil {
		return errors.New("failed to parse download ID from URL: " + err.Error())
	}

	u.Path = ""
	server := u.String()

	transfer := downloadTransfer{file: file, fileID: fileID, from: &from}

	pos, err := file.Seek(0, 2 /* from end */)
	if err != nil {
		return err
	}
	transfer.resume = pos

	return c.transferDetachment(out, server, transfer, id, killChan)
}

func (c *client) transferDetachment(out chan interface{}, server string, transfer detachmentTransfer, id uint64, killChan chan bool) error {
	var transferred, total int64

	sendStatus := func(s string) {
		select {
		case out <- DetachmentProgress{
			id:     id,
			done:   uint64(transferred),
			total:  uint64(total),
			status: s,
		}:
			break
		default:
		}
	}

	const initialBackoff = 10 * time.Second
	const maxBackoff = 5 * time.Minute
	backoff := initialBackoff

	for {
		sendStatus("Connecting")

		conn, err := c.dialServer(server, false)
		if err != nil {
			c.log.Printf("Failed to connect to %s: %s", c.server, err)
			sendStatus("Waiting to reconnect")

			select {
			case <-time.After(backoff):
				break
			case <-killChan:
				return backgroundCanceledError
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = initialBackoff

		sendStatus("Requesting transfer")
		if err := conn.WriteProto(transfer.Request()); err != nil {
			c.log.Printf("Failed to write request to %s: %s", c.server, err)
			conn.Close()
			continue
		}

		reply := new(pond.Reply)
		if err := conn.ReadProto(reply); err != nil {
			c.log.Printf("Failed to read reply from %s: %s", c.server, err)
			conn.Close()
			continue
		}

		if reply.Status != nil && *reply.Status == pond.Reply_RESUME_PAST_END_OF_FILE {
			conn.Close()
			return nil
		}

		if err := replyToError(reply); err != nil {
			c.log.Printf("Request failed: %s", err)
			conn.Close()
			return err
		}

		var file *os.File
		var isUpload, isComplete bool
		if file, isUpload, total, isComplete, err = transfer.ProcessReply(reply); err != nil {
			c.log.Printf("Request failed: %s", err)
			conn.Close()
			return err
		}
		if isComplete {
			conn.Close()
			return nil
		}

		var in io.Reader
		var out io.Writer
		if isUpload {
			out = conn
			in = file
		} else {
			out = file
			in = conn
		}

		buf := make([]byte, 16*1024)
		var lastUpdate time.Time

		for {
			select {
			case <-killChan:
				conn.Close()
				return backgroundCanceledError
			default:
				break
			}

			conn.SetDeadline(time.Now().Add(30 * time.Second))

			n, err := in.Read(buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				conn.Close()
				if isUpload {
					err = fmt.Errorf("failed to read during transfer: %s", err)
					c.log.Printf("%s", err)
					return err
				}
				// Read errors from the network are transient.
				continue
			}

			n, err = out.Write(buf[:n])
			if err != nil {
				conn.Close()
				if !isUpload {
					err = fmt.Errorf("failed to write during download: %s", err)
					c.log.Printf("%s", err)
					return err
				}
				// Write errors to the network are transient.
				continue
			}

			transferred += int64(n)
			if transferred > total {
				err = errors.New("transferred more than the expected amount")
				conn.Close()
				c.log.Printf("%s", err)
				return err
			}
			now := time.Now()
			if lastUpdate.IsZero() || now.Sub(lastUpdate) > 500*time.Millisecond {
				lastUpdate = now
				sendStatus("")
			}

			time.Sleep(5 * time.Millisecond)
		}

		if transferred < total {
			conn.Close()
			continue
		}

		ok := transfer.Complete(conn)
		conn.Close()
		if ok {
			return nil
		}
	}

	return nil
}
