package weave

import (
	"code.google.com/p/gopacket"
	"code.google.com/p/gopacket/layers"
	"log"
	"net"
	"syscall"
	"time"
)

func (conn *LocalConnection) ensureForwarders() error {
	if conn.forwardChan != nil || conn.forwardChanDF != nil {
		return nil
	}
	// only thing that can error, so do it earlier
	rawUDPSender, err := NewRawUDPSender(conn)
	if err != nil {
		return err
	}

	usingPassword := conn.SessionKey != nil
	var encryptor, encryptorDF Encryptor
	if usingPassword {
		encryptor = NewNaClEncryptor(conn.local.NameByte, conn, false)
		encryptorDF = NewNaClEncryptor(conn.local.NameByte, conn, true)
	} else {
		encryptor = NewNonEncryptor(conn.local.NameByte)
		encryptorDF = NewNonEncryptor(conn.local.NameByte)
	}

	// The forward chans in the conn struct are read by other
	// processes, so we have to use locks.
	var (
		forwardChan   = make(chan *ForwardedFrame, ChannelSize)
		forwardChanDF = make(chan *ForwardedFrame, ChannelSize)
		stopForward   = make(chan interface{}, 0)
		stopForwardDF = make(chan interface{}, 0)
	)
	conn.Lock()
	conn.forwardChan = forwardChan
	conn.forwardChanDF = forwardChanDF
	conn.stopForward = stopForward
	conn.stopForwardDF = stopForwardDF
	conn.Unlock()
	go conn.forwarderLoop(forwardChan, stopForward, encryptor, NewSimpleUDPSender(conn), DefaultPMTU)
	go conn.forwarderLoop(forwardChanDF, stopForwardDF, encryptorDF, rawUDPSender, conn.clientPMTU)

	return nil
}

func (conn *LocalConnection) stopForwarders() {
	conn.Lock()
	conn.forwardChan = nil
	conn.forwardChanDF = nil
	conn.Unlock()
	// Now signal the forwarder loops to exit. They will drain the
	// forwarder chans in order to unblock any router processes
	// blocked on sending.
	if conn.stopForward != nil {
		conn.stopForward <- nil
		conn.stopForwardDF <- nil
	}
}

func (conn *LocalConnection) forwarderLoop(forwardCh <-chan *ForwardedFrame, stop <-chan interface{}, enc Encryptor, udpSender UDPSender, pmtu int) {
	defer udpSender.Shutdown()
	// The pmtu governs the max size of msg we can pass to the
	// udpSender. udpSender will add in the UDP/IP headers
	var pmtuVerifyTick <-chan time.Time
	var clientPMTU int
	maxPayload := pmtu - UDPOverhead
	updateClientPMTU := func() {
		conn.setClientPMTU(clientPMTU)
		pmtuVerifyFrame := &ForwardedFrame{
			srcPeer: conn.local,
			dstPeer: conn.remote,
			frame:   make([]byte, clientPMTU+EthernetOverhead)}
		for cnt := 0; cnt < 10; cnt += 1 {
			enc.AppendFrame(pmtuVerifyFrame)
			udpSender.Send(enc.Bytes())
		}
		pmtuVerifyTick = time.After(PMTUVerifyTimeout)
	}
	appendFrame := func(frame *ForwardedFrame) bool {
		frameLen := len(frame.frame)
		if enc.TotalLen()+enc.FrameOverhead()+frameLen > maxPayload {
			return false
		}
		enc.AppendFrame(frame)
		return true
	}
	flush := func() {
		err := udpSender.Send(enc.Bytes())
		if err != nil {
			if ftbe, ok := err.(FrameTooBigError); ok {
				maxPayload = ftbe.PMTU - UDPOverhead
				clientPMTU = maxPayload - enc.PacketOverhead() - enc.FrameOverhead() - EthernetOverhead
				updateClientPMTU()
			} else if PosixError(err) == syscall.ENOBUFS {
				// TODO handle this better
			} else {
				conn.CheckFatal(err)
			}
		}
	}
	logDrop := func(frame *ForwardedFrame) {
		conn.log("dropping too big frame during forwarding: frame len:", len(frame.frame), "; PMTU:", pmtu)
	}
	// We want to drain before exiting otherwise we could get the
	// packet sniffer or udp listener blocked on sending to a full
	// chan
	drain := func() {
		for {
			select {
			case <-forwardCh:
			default:
				return
			}
		}
	}
	var flushed, ok bool
	var frame *ForwardedFrame
	for {
		flushed = false
		select {
		case <-stop:
			drain()
			return
		case <-pmtuVerifyTick:
			// We only do this case here when we know the buffers are
			// all empty so that we don't risk appending verify-frames
			// to other data.
			pmtuVerifyTick = nil
			if !conn.isClientPMTUVerfied() {
				clientPMTU -= 10
				updateClientPMTU()
			}
		case frame = <-forwardCh:
			if !appendFrame(frame) {
				logDrop(frame)
				continue
			}
			for !flushed {
				select {
				case frame, ok = <-forwardCh:
					if !ok {
						return
					}
					if !appendFrame(frame) {
						flush()
						if !appendFrame(frame) {
							logDrop(frame)
							flushed = true
						}
					}
				default:
					flush()
					flushed = true
				}
			}
		}
	}
}

// Called from peer.Relay[Broadcast] which is itself invoked from
// router (both UDP listener process and sniffer process). Also called
// from connection's heartbeat process, and from the connection's TCP
// receiver process.
func (conn *LocalConnection) Forward(df bool, frame *ForwardedFrame, dec *EthernetDecoder) error {
	conn.RLock()
	var (
		forwardChan   = conn.forwardChan
		forwardChanDF = conn.forwardChanDF
		clientPMTU    = conn.clientPMTU
		stackFrag     = conn.stackFrag
	)
	conn.RUnlock()

	if forwardChan == nil || forwardChanDF == nil {
		conn.log("Cannot forward frame yet - awaiting contact")
		return nil
	}
	if df {
		if len(frame.frame)-EthernetOverhead <= clientPMTU {
			forwardChanDF <- frame
			return nil
		} else {
			return FrameTooBigError{PMTU: clientPMTU, frame: frame}
		}
	} else {
		if stackFrag || dec == nil || len(dec.decoded) < 2 {
			forwardChan <- frame
			return nil
		}
		// Don't have trustworthy stack, so we're going to have to
		// send it DF in any case.
		if len(frame.frame)-EthernetOverhead <= clientPMTU {
			forwardChanDF <- frame
			return nil
		}
		conn.Router.LogFrame("Fragmenting", frame.frame, &dec.eth)
		// We can't trust the stack to fragment, we have IP, and we
		// have a frame that's too big for the MTU, so we have to
		// fragment it ourself.
		return fragment(dec.eth, dec.ip, clientPMTU, frame, func(segFrame *ForwardedFrame) {
			forwardChanDF <- segFrame
		})
	}
}

func fragment(eth layers.Ethernet, ip layers.IPv4, pmtu int, frame *ForwardedFrame, forward func(*ForwardedFrame)) error {
	// We are not doing any sort of NAT, so we don't need to worry
	// about checksums of IP payload (eg UDP checksum).
	headerSize := int(ip.IHL) * 4
	// &^ is bit clear (AND NOT). So here we're clearing the lowest 3
	// bits.
	maxSegmentSize := (pmtu - EthernetOverhead - headerSize) &^ 7
	opts := gopacket.SerializeOptions{
		FixLengths:       false,
		ComputeChecksums: true}
	payloadSize := int(ip.Length) - headerSize
	payload := ip.BaseLayer.Payload[:payloadSize]
	offsetBase := int(ip.FragOffset) << 3
	origFlags := ip.Flags
	ip.Flags = ip.Flags | layers.IPv4MoreFragments
	ip.Length = uint16(headerSize + maxSegmentSize)
	if eth.EthernetType == layers.EthernetTypeLLC {
		// using LLC, so must set eth length correctly. eth length
		// is just the length of the payload
		eth.Length = ip.Length
	} else {
		eth.Length = 0
	}
	for offset := 0; offset < payloadSize; offset += maxSegmentSize {
		var segmentPayload []byte
		if len(payload) <= maxSegmentSize {
			// last one
			segmentPayload = payload
			ip.Length = uint16(len(payload) + headerSize)
			ip.Flags = origFlags
			if eth.EthernetType == layers.EthernetTypeLLC {
				eth.Length = ip.Length
			} else {
				eth.Length = 0
			}
		} else {
			segmentPayload = payload[:maxSegmentSize]
			payload = payload[maxSegmentSize:]
		}
		ip.FragOffset = uint16((offset + offsetBase) >> 3)
		buf := gopacket.NewSerializeBuffer()
		segPayload := gopacket.Payload(segmentPayload)
		err := gopacket.SerializeLayers(buf, opts, &eth, &ip, &segPayload)
		if err != nil {
			return err
		}
		// make copies of the frame we received
		var segFrame ForwardedFrame = *frame
		segFrame.frame = buf.Bytes()
		forward(&segFrame)
	}
	return nil
}

// UDP Senders

func NewSimpleUDPSender(conn *LocalConnection) *SimpleUDPSender {
	return &SimpleUDPSender{udpConn: conn.Router.UDPListener, conn: conn}
}

func (sender *SimpleUDPSender) Send(msg []byte) error {
	_, err := sender.udpConn.WriteToUDP(msg, sender.conn.RemoteUDPAddr())
	return err
}

func (sender *SimpleUDPSender) Shutdown() error {
	return nil
}

func NewRawUDPSender(conn *LocalConnection) (*RawUDPSender, error) {
	ipSocket, err := dialIP(conn)
	if err != nil {
		return nil, err
	}
	udpHeader := &layers.UDP{SrcPort: layers.UDPPort(Port)}
	ipBuf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths: true,
		// UDP header is calculated with a phantom IP
		// header. Yes, it's totally nuts. Thankfully, for UDP
		// over IPv4, the checksum is optional. It's not
		// optional for IPv6, but we'll ignore that for
		// now. TODO
		ComputeChecksums: false}

	return &RawUDPSender{
		ipBuf:     ipBuf,
		opts:      opts,
		udpHeader: udpHeader,
		socket:    ipSocket,
		conn:      conn}, nil
}

func (sender *RawUDPSender) Send(msg []byte) error {
	payload := gopacket.Payload(msg)
	sender.udpHeader.DstPort = layers.UDPPort(sender.conn.RemoteUDPAddr().Port)

	err := gopacket.SerializeLayers(sender.ipBuf, sender.opts, sender.udpHeader, &payload)
	if err != nil {
		return err
	}
	packet := sender.ipBuf.Bytes()
	_, err = sender.socket.Write(packet)
	if err != nil {
		if PosixError(err) == syscall.EMSGSIZE {
			f, err := sender.socket.File()
			if err != nil {
				return err
			}
			defer f.Close()
			fd := int(f.Fd())
			log.Println("EMSGSIZE on send, expecting PMTU update (IP packet was",
				len(packet), "bytes, payload was", len(msg), "bytes)")
			pmtu, err := syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU)
			if err != nil {
				return err
			}
			return FrameTooBigError{PMTU: pmtu}
		} else {
			return err
		}
	}
	return nil
}

func (sender *RawUDPSender) Shutdown() error {
	defer func() { sender.socket = nil }()
	return sender.socket.Close()
}

func dialIP(conn *LocalConnection) (*net.IPConn, error) {
	ipLocalAddr, err := ipAddr(conn.TCPConn.LocalAddr())
	if err != nil {
		return nil, err
	}
	ipRemoteAddr, err := ipAddr(conn.TCPConn.RemoteAddr())
	if err != nil {
		return nil, err
	}
	ipSocket, err := net.DialIP("ip4:UDP", ipLocalAddr, ipRemoteAddr)
	if err != nil {
		return nil, err
	}
	f, err := ipSocket.File()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd := int(f.Fd())
	// This Makes sure all packets we send out have DF set on them.
	err = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER, syscall.IP_PMTUDISC_DO)
	if err != nil {
		return nil, err
	}
	return ipSocket, nil
}

func ipAddr(addr net.Addr) (*net.IPAddr, error) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, err
	}
	return &net.IPAddr{
		IP:   net.ParseIP(host),
		Zone: ""}, nil
}