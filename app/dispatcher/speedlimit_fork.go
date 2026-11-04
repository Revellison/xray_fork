package dispatcher

import (
	"context"

	"github.com/xtls/xray-core/app/speedlimit"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
)

// limitPipeLink limits links created by Dispatch. It wraps the stats writers, so user
// statistics count traffic that has passed the limiter. Without a "speedLimit" section,
// or for users who can never be limited, it changes nothing.
func limitPipeLink(ctx context.Context, inbound *session.Inbound, email string, network net.Network, inboundLink, outboundLink *transport.Link) {
	us := speedlimit.Default().User(email)
	if !us.MayLimit() {
		return
	}
	disableSplice(inbound)
	inboundLink.Writer = us.WrapUp(ctx, inboundLink.Writer, network)
	outboundLink.Writer = us.WrapDown(ctx, outboundLink.Writer, network)
}

// limitDirectLink limits links handed to DispatchLink: VLESS over any transport, SOCKS,
// HTTP, Hysteria, TUN and others. These never pass through getLink and have no uplink
// writer, so uplink is limited on the reader. Call it before TimeoutWrapperReader wraps
// link.Reader, because the sniffer and the stats counter expect that outer type.
func limitDirectLink(ctx context.Context, inbound *session.Inbound, user *protocol.MemoryUser, link *transport.Link) {
	if user == nil || len(user.Email) == 0 {
		return
	}
	us := speedlimit.Default().User(user.Email)
	if !us.MayLimit() {
		return
	}
	network := net.Network_TCP
	if outbounds := session.OutboundsFromContext(ctx); len(outbounds) > 0 {
		network = outbounds[len(outbounds)-1].Target.Network
	}
	disableSplice(inbound)
	link.Reader = us.WrapUpReader(ctx, link.Reader, network)
	link.Writer = us.WrapDown(ctx, link.Writer, network)
}

// disableSplice keeps limited traffic out of kernel splice in
// proxy.CopyRawConnIfExist, which copies between sockets and skips every writer.
func disableSplice(inbound *session.Inbound) {
	if inbound != nil && inbound.CanSpliceCopy != 3 {
		inbound.CanSpliceCopy = 3
	}
}

