package hecommplatform

import (
	"context"
	"crypto/tls"
	"log"
	"net"

	"fmt"

	"github.com/brocaar/lorawan"
	"github.com/joriwind/hecomm-fog/hecomm"
	"github.com/joriwind/lora-app-server/internal/common"
	"github.com/joriwind/lora-app-server/internal/downlink"
	"github.com/joriwind/lora-app-server/internal/storage"
	"github.com/monnand/dhkx"
)

//Server Struct defining the hecomm server
type Server struct {
	ctx         context.Context
	commonCtx   common.Context
	address     string
	credentials tls.Certificate
	fogAddress  string
	links       map[lorawan.EUI64]Link
}

//Link A link between node and the contract
type Link struct {
	linkContract hecomm.LinkContract
	osSKey       [16]byte
}

//NewServer Create new hecomm server API
func NewServer(ctx context.Context, commonCtx common.Context, address string, credentials tls.Certificate, fogAddress string) (*Server, error) {
	var srv Server

	srv.ctx = ctx
	srv.commonCtx = commonCtx
	srv.address = address
	srv.credentials = credentials
	srv.fogAddress = fogAddress

	return &srv, nil
}

//Start Start listening
func (s *Server) Start() error {
	config := tls.Config{Certificates: []tls.Certificate{s.credentials}}
	listener, err := tls.Listen("tcp", "localhost:8000", &config)
	defer listener.Close()
	if err != nil {
		return err
	}
	chanConn := make(chan net.Conn, 1)
	//Listen on tls port
	for {
		//Wait for connection!
		go func(listener net.Listener, chanConn chan net.Conn) {
			conn, err := listener.Accept()

			if err != nil {
				log.Printf("hecommplatform server: did not accept: %v\n", err)
				return
			}
			chanConn <- conn
			return
		}(listener, chanConn)

		//Check if connection available or context
		select {
		case conn := <-chanConn:
			if conn.RemoteAddr().String() == s.fogAddress {
				s.handleProviderConnection(conn)
			} else {
				log.Printf("hecommplatform server: wrong connection: %v\n", conn)
			}
		case <-s.ctx.Done():
			return nil
		}

	}

}

func (s *Server) handleProviderConnection(conn net.Conn) {
	buf := make([]byte, 2048)
	//The to be created link
	var link Link
	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("hecommplatform server: handleConnection: could not read: %v\n", err)
			break
		}

		message, err := hecomm.GetMessage(buf[:n])
		if err != nil {
			log.Fatalln("Unable to decipher bytes on hecomm channel!")
		}

		switch message.FPort {
		case hecomm.FPortLinkReq:
			//Link request from fog
			lc, err := message.GetLinkContract()
			if err != nil {
				log.Printf("hecommplatform server: handleConnection: unvalid LinkContract: error: %v\n", err)
			}
			if lc.Linked {
				log.Printf("Unexpected LinkContract in LinkReq, is already linked: %v\n", lc)
			}

			//Convert eui
			eui := ConvertHecommDevEUIToPlatformDevEUI(lc.ProvDevEUI)

			//Find possible node
			node, err := storage.GetNode(s.commonCtx.DB, eui)
			if err != nil {
				log.Fatalf("hecommplatform server: handleConnection: could not resolve deveui: %v\n", err)
			}
			//Check if valid node is found --> node.DevEUI not nil or something
			if node.DevEUI != eui {
				log.Printf("hecommplatform server: handleconnection: could not find node\n")
				rsp, err := hecomm.NewResponse(false)
				if err != nil {
					log.Fatalf("Failed to create response: error: %v\n", err)
					return
				}
				_, err = conn.Write(rsp)
				if err != nil {
					log.Fatalf("hecommplatform server: handleConnection: failed to send response: %v\n", err)
					return
				}
				return
			}
			//Check if node already has connection!
			if _, ok := s.links[eui]; ok {
				log.Printf("Unable to connect to this node, already connected: %v\n", lc)
				rsp, err := hecomm.NewResponse(false)
				if err != nil {
					log.Fatalf("Failed to create response: error: %v\n", err)
					return
				}
				_, err = conn.Write(rsp)
				if err != nil {
					log.Fatalf("hecommplatform server: handleConnection: failed to send response: %v\n", err)
					return
				}
				break

			}

			//Send positive response and start PK state
			rsp, err := hecomm.NewResponse(true)
			if err != nil {
				log.Fatalf("hecommplatform server: handleConnection: failed to create response: %v\n", err)
				return
			}
			_, err = conn.Write(rsp)
			if err != nil {
				log.Fatalf("hecommplatform server: handleConnection: failed to send response: %v\n", err)
				return
			}

			//Add to temp link
			//Check if already started linking
			if link.linkContract.ProvDevEUI != nil {
				log.Printf("Already start linking!?: Link: %v LinkContract: %v\n", link, lc)
				break
			}
			link.linkContract = *lc

		case hecomm.FPortLinkState:
			if link.linkContract.ProvDevEUI == nil {
				log.Printf("The link protocol has not started yet?: %v\n", link)
			}
			err := bob(&link, conn, *message)
			if err != nil {
				log.Printf("DH protocol error: %v\n", err)
				break
			}

		case hecomm.FPortLinkSet:
			//Link set from fog
			lc, err := message.GetLinkContract()
			if err != nil {
				log.Printf("hecommplatform server: handleConnection: unvalid LinkContract: error: %v\n", err)
			}

			//Convert identifier
			eui := ConvertHecommDevEUIToPlatformDevEUI(lc.ProvDevEUI)

			//Check corresponds with active linking
			for index, b := range link.linkContract.ProvDevEUI {
				if b != lc.ProvDevEUI[index] {
					log.Fatalf("LinkSet does not correspond with active link!: Link: %v, LinkSet: %v\n", link, lc)
					return
				}
			}

			//Check if shared key is set
			if link.osSKey == [16]byte{} {
				log.Printf("Active link does not have a shared key: %v", link)
				break
			}

			//Push key to node
			//Get corresponding node
			node, err := storage.GetNode(s.commonCtx.DB, eui)
			if err != nil {
				log.Printf("get node error: %s\n", err)

				//Send bad response
				bytes, err := hecomm.NewResponse(false)
				if err != nil {
					log.Fatalf("Could not create hecomm false response: %v\n", err)
				}
				conn.Write(bytes)
				return
			}
			//Add item, pushing key down to node
			item := &storage.DownlinkQueueItem{Confirmed: true, Data: link.osSKey[:], DevEUI: eui, FPort: 254, Reference: "key"}
			err = downlink.HandleDownlinkQueueItem(s.commonCtx, node, item)
			if err != nil {
				fmt.Printf("hecommplatform server: failed to push osSKey: %v\n", err)

				//Send bad response
				bytes, err := hecomm.NewResponse(false)
				if err != nil {
					log.Fatalf("Could not create hecomm false response: %v\n", err)
				}
				conn.Write(bytes)
				return
			}
			//Define link in state
			s.links[eui] = link
			log.Printf("Link is set: %v\n", link)
			//Clear link
			link = Link{}

			//Send ok response
			bytes, err := hecomm.NewResponse(true)
			if err != nil {
				log.Fatalf("Could not create hecomm true response: %v\n", err)
			}
			conn.Write(bytes)

		default:
			log.Printf("Unexpected FPort in hecomm message: %v\n", message)
		}

	}
}

//alice Initiator side from DH perspective
func alice(link *Link, conn net.Conn, buf []byte) error {
	//Get default group
	g, err := dhkx.GetGroup(0)
	if err != nil {
		//log.Fatalf("Could not create group(DH): %v\n", err)
		return err
	}

	//Generate a private key from the group, use the default RNG
	priv, err := g.GeneratePrivateKey(nil)
	if err != nil {
		//log.Fatalf("Could not generate private key(DH): %v\n", err)
		return err
	}

	//Get public key from private key
	pub := priv.Bytes()

	bytes, err := hecomm.NewMessage(hecomm.FPortLinkState, pub)
	if err != nil {
		//log.Fatalf("Could not create hecomm message: %v\n", err)
		return err
	}

	_, err = conn.Write(bytes)
	if err != nil {
		return err
	}

	//Receive bytes from bob, containing bob's pub key
	n, err := conn.Read(buf)
	if err != nil {
		//log.Fatalf("Could not read from connection: %v\n", err)
		return err
	}

	//Recover Bob's public key
	bobPubKey := dhkx.NewPublicKey(buf[:n])

	//Compute shared key
	k, err := g.ComputeKey(bobPubKey, priv)
	if err != nil {
		//log.Fatalf("Could not compute key(DH): %v\n", err)
		return err
	}
	//Get the key in []byte form
	key := k.Bytes()
	copy(link.osSKey[:], key[:16])
	return nil
}

//bob Receiver side form DH perspective, linkstate message already received
func bob(link *Link, conn net.Conn, message hecomm.Message) error {
	g, err := dhkx.GetGroup(0)
	if err != nil {
		//log.Fatalf("Could not create group(DH): %v\n", err)
		return err
	}

	//Generate a private key from the group, use the default RNG
	priv, err := g.GeneratePrivateKey(nil)
	if err != nil {
		//log.Fatalf("Could not generate private key(DH): %v\n", err)
		return err
	}

	//Get public key from private key
	pub := priv.Bytes()

	//Already received bytes from alice, containing alice's pub key
	//Check if right FPort
	if message.FPort != hecomm.FPortLinkState {
		//log.Fatalf("Received message in wrong FPort: %v\n", message)
		return fmt.Errorf("Received message in wrong FPort: %v", message)
	}

	//Create message containig bob's pub key
	bytes, err := hecomm.NewMessage(hecomm.FPortLinkState, pub)
	if err != nil {
		//log.Fatalf("Could not create hecomm message: %v\n", err)
		return err
	}

	_, err = conn.Write(bytes)
	if err != nil {
		return err
	}

	//Recover alice's public key
	alicePubKey := dhkx.NewPublicKey(message.Data)

	//Compute shared key
	k, err := g.ComputeKey(alicePubKey, priv)
	if err != nil {
		//log.Fatalf("Could not compute key(DH): %v\n", err)
		return err
	}
	//Get the key in []byte form
	key := k.Bytes()
	copy(link.osSKey[:], key[:16])
	return nil
}
