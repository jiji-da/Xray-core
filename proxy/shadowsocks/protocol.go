package shadowsocks

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"hash/crc32"
	"io"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/crypto"
	"github.com/xtls/xray-core/common/drain"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
)

const (
	Version = 1
)

var addrParser = protocol.NewAddressParser(
	protocol.AddressFamilyByte(0x01, net.AddressFamilyIPv4),
	protocol.AddressFamilyByte(0x04, net.AddressFamilyIPv6),
	protocol.AddressFamilyByte(0x03, net.AddressFamilyDomain),
	protocol.WithAddressTypeParser(func(b byte) byte {
		return b & 0x0F
	}),
)

type FullReader struct {
	reader io.Reader
	buffer []byte
}

func (r *FullReader) Read(p []byte) (n int, err error) {
	if r.buffer != nil {
		n := copy(p, r.buffer)
		if n == len(r.buffer) {
			r.buffer = nil
		} else {
			r.buffer = r.buffer[n:]
		}
		if n == len(p) {
			return n, nil
		} else {
			m, err := r.reader.Read(p[n:])
			return n + m, err
		}
	}
	return r.reader.Read(p)
}

// ReadTCPSession reads a Shadowsocks TCP session from the given reader, returns its header and remaining parts.
func ReadTCPSession(validator *Validator, reader io.Reader) (*protocol.RequestHeader, buf.Reader, error) {
	behaviorSeed := validator.GetBehaviorSeed()
	drainer, errDrain := drain.NewBehaviorSeedLimitedDrainer(int64(behaviorSeed), 16+38, 3266, 64)

	if errDrain != nil {
		return nil, nil, newError("failed to initialize drainer").Base(errDrain)
	}

	// read fake-sni
	fakeSniBuffer := buf.New()
	for i := 0; i < 50; i++ {
		if _, err := fakeSniBuffer.ReadFullFrom(reader, 1); err != nil {
			return nil, nil, drain.WithError(drainer, reader, newError("failed to read fake sin").Base(err))
		}
		if fakeSniBuffer.Len() > 1 {
			magicByte := fakeSniBuffer.BytesFrom(fakeSniBuffer.Len() - 2)
			if string(magicByte[:]) == "\r\n" {
				break
			}
		}
	}
	fakeSniBuffer.Release()

	var r buf.Reader
	buffer := buf.New()
	defer buffer.Release()

	if _, err := buffer.ReadFullFrom(reader, 50); err != nil {
		drainer.AcknowledgeReceive(int(buffer.Len()))
		return nil, nil, drain.WithError(drainer, reader, newError("failed to read 50 bytes").Base(err))
	}

	bs := buffer.Bytes()
	user, aead, _, ivLen, err := validator.Get(bs, protocol.RequestCommandTCP)

	switch err {
	case ErrNotFound:
		drainer.AcknowledgeReceive(int(buffer.Len()))
		return nil, nil, drain.WithError(drainer, reader, newError("failed to match an user").Base(err))
	case ErrIVNotUnique:
		drainer.AcknowledgeReceive(int(buffer.Len()))
		return nil, nil, drain.WithError(drainer, reader, newError("failed iv check").Base(err))
	default:
		reader = &FullReader{reader, bs[ivLen:]}
		drainer.AcknowledgeReceive(int(ivLen))

		if aead != nil {
			auth := &crypto.AEADAuthenticator{
				AEAD:           aead,
				NonceGenerator: crypto.GenerateAEADNonceWithSize(aead.NonceSize()),
			}
			r = crypto.NewAuthenticationReader(auth, &crypto.AEADChunkSizeParser{
				Auth: auth,
			}, reader, protocol.TransferTypeStream, nil)
		} else {
			account := user.Account.(*MemoryAccount)
			iv := append([]byte(nil), buffer.BytesTo(ivLen)...)
			r, err = account.Cipher.NewDecryptionReader(account.Key, iv, reader)
			if err != nil {
				return nil, nil, drain.WithError(drainer, reader, newError("failed to initialize decoding stream").Base(err).AtError())
			}
		}
	}

	br := &buf.BufferedReader{Reader: r}

	request := &protocol.RequestHeader{
		Version: Version,
		User:    user,
		Command: protocol.RequestCommandTCP,
	}

	buffer.Clear()

	addr, port, err := addrParser.ReadAddressPort(buffer, br)
	if err != nil {
		drainer.AcknowledgeReceive(int(buffer.Len()))
		return nil, nil, drain.WithError(drainer, reader, newError("failed to read address").Base(err))
	}

	request.Address = addr
	request.Port = port

	if request.Address == nil {
		drainer.AcknowledgeReceive(int(buffer.Len()))
		return nil, nil, drain.WithError(drainer, reader, newError("invalid remote address."))
	}

	return request, br, nil
}

// WriteTCPRequest writes Shadowsocks request into the given writer, and returns a writer for body.
func WriteTCPRequest(request *protocol.RequestHeader, writer io.Writer) (buf.Writer, error) {
	user := request.User
	account := user.Account.(*MemoryAccount)

	var iv []byte
	if account.Cipher.IVSize() > 0 {
		iv = make([]byte, account.Cipher.IVSize())
		common.Must2(rand.Read(iv))
		if ivError := account.CheckIV(iv); ivError != nil {
			return nil, newError("failed to mark outgoing iv").Base(ivError)
		}
		if err := buf.WriteAllBytes(writer, iv, nil); err != nil {
			return nil, newError("failed to write IV")
		}
	}

	w, err := account.Cipher.NewEncryptionWriter(account.Key, iv, writer)
	if err != nil {
		return nil, newError("failed to create encoding stream").Base(err).AtError()
	}

	header := buf.New()

	if err := addrParser.WriteAddressPort(header, request.Address, request.Port); err != nil {
		return nil, newError("failed to write address").Base(err)
	}

	if err := w.WriteMultiBuffer(buf.MultiBuffer{header}); err != nil {
		return nil, newError("failed to write header").Base(err)
	}

	return w, nil
}

func ReadTCPResponse(user *protocol.MemoryUser, reader io.Reader) (buf.Reader, error) {
	account := user.Account.(*MemoryAccount)

	hashkdf := hmac.New(sha256.New, []byte("SSBSKDF"))
	hashkdf.Write(account.Key)

	behaviorSeed := crc32.ChecksumIEEE(hashkdf.Sum(nil))

	drainer, err := drain.NewBehaviorSeedLimitedDrainer(int64(behaviorSeed), 16+38, 3266, 64)
	if err != nil {
		return nil, newError("failed to initialize drainer").Base(err)
	}

	var iv []byte
	if account.Cipher.IVSize() > 0 {
		iv = make([]byte, account.Cipher.IVSize())
		if n, err := io.ReadFull(reader, iv); err != nil {
			return nil, newError("failed to read IV").Base(err)
		} else { // nolint: golint
			drainer.AcknowledgeReceive(n)
		}
	}

	if ivError := account.CheckIV(iv); ivError != nil {
		return nil, drain.WithError(drainer, reader, newError("failed iv check").Base(ivError))
	}

	return account.Cipher.NewDecryptionReader(account.Key, iv, reader)
}

func WriteTCPResponse(request *protocol.RequestHeader, writer io.Writer) (buf.Writer, error) {
	user := request.User
	account := user.Account.(*MemoryAccount)

	var iv []byte
	if account.Cipher.IVSize() > 0 {
		iv = make([]byte, account.Cipher.IVSize())
		common.Must2(rand.Read(iv))
		if ivError := account.CheckIV(iv); ivError != nil {
			return nil, newError("failed to mark outgoing iv").Base(ivError)
		}
		if err := buf.WriteAllBytes(writer, iv, nil); err != nil {
			return nil, newError("failed to write IV.").Base(err)
		}
	}

	return account.Cipher.NewEncryptionWriter(account.Key, iv, writer)
}

func EncodeUDPPacket(request *protocol.RequestHeader, payload []byte) (*buf.Buffer, error) {
	user := request.User
	account := user.Account.(*MemoryAccount)

	buffer := buf.New()
	ivLen := account.Cipher.IVSize()
	if ivLen > 0 {
		common.Must2(buffer.ReadFullFrom(rand.Reader, ivLen))
	}

	if err := addrParser.WriteAddressPort(buffer, request.Address, request.Port); err != nil {
		return nil, newError("failed to write address").Base(err)
	}

	buffer.Write(payload)

	if err := account.Cipher.EncodePacket(account.Key, buffer); err != nil {
		return nil, newError("failed to encrypt UDP payload").Base(err)
	}

	if request.Sni != "" {
		newBuffer := buf.New()
		newBuffer.Write([]byte(request.Sni))
		newBuffer.Write(buffer.Bytes())
		buffer.Release()
		return newBuffer, nil
	} else {
		return buffer, nil
	}
}

func DecodeUDPPacket(validator *Validator, payload *buf.Buffer) (*protocol.RequestHeader, *buf.Buffer, error) {
	//deal fake-sni
	obs := payload.Bytes()
	if bytes.HasPrefix(obs, []byte{70, 97, 107, 101, 45, 83, 110, 105, 58}) {
		_, nbs, _ := bytes.Cut(obs, []byte{13, 10})
		payload.Release()
		payload = buf.New()
		payload.Write(nbs)
	}

	bs := payload.Bytes()
	if len(bs) <= 32 {
		return nil, nil, newError("len(bs) <= 32")
	}

	user, _, d, _, err := validator.Get(bs, protocol.RequestCommandUDP)
	switch err {
	case ErrIVNotUnique:
		return nil, nil, newError("failed iv check").Base(err)
	case ErrNotFound:
		return nil, nil, newError("failed to match an user").Base(err)
	default:
		account := user.Account.(*MemoryAccount)
		if account.Cipher.IsAEAD() {
			payload.Clear()
			payload.Write(d)
		} else {
			if account.Cipher.IVSize() > 0 {
				iv := make([]byte, account.Cipher.IVSize())
				copy(iv, payload.BytesTo(account.Cipher.IVSize()))
			}
			if err = account.Cipher.DecodePacket(account.Key, payload); err != nil {
				return nil, nil, newError("failed to decrypt UDP payload").Base(err)
			}
		}
	}

	request := &protocol.RequestHeader{
		Version: Version,
		User:    user,
		Command: protocol.RequestCommandUDP,
	}

	payload.SetByte(0, payload.Byte(0)&0x0F)

	addr, port, err := addrParser.ReadAddressPort(nil, payload)
	if err != nil {
		return nil, nil, newError("failed to parse address").Base(err)
	}

	request.Address = addr
	request.Port = port

	return request, payload, nil
}

type UDPReader struct {
	Reader io.Reader
	User   *protocol.MemoryUser
}

func (v *UDPReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	buffer := buf.New()
	_, err := buffer.ReadFrom(v.Reader)
	if err != nil {
		buffer.Release()
		return nil, err
	}
	validator := new(Validator)
	validator.Add(v.User)

	u, payload, err := DecodeUDPPacket(validator, buffer)
	if err != nil {
		buffer.Release()
		return nil, err
	}
	dest := u.Destination()
	payload.UDP = &dest
	return buf.MultiBuffer{payload}, nil
}

type UDPWriter struct {
	Writer  io.Writer
	Request *protocol.RequestHeader
}

func (w *UDPWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	for {
		mb2, b := buf.SplitFirst(mb)
		mb = mb2
		if b == nil {
			break
		}
		request := w.Request
		if b.UDP != nil {
			request = &protocol.RequestHeader{
				User:       w.Request.User,
				Address:    b.UDP.Address,
				Port:       b.UDP.Port,
				Sni:        w.Request.Sni,
				UdpSpeeder: w.Request.UdpSpeeder,
			}
		}
		packet, err := EncodeUDPPacket(request, b.Bytes())
		b.Release()
		if err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
		if request.UdpSpeeder > 0 {
			for i := uint32(0); i < request.UdpSpeeder; i++ {
				_, err = w.Writer.Write(packet.Bytes())
			}
		} else {
			_, err = w.Writer.Write(packet.Bytes())
		}
		packet.Release()
		if err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
	}
	return nil
}
