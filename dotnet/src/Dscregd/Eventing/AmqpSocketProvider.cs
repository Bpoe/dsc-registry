namespace Dscregd.Eventing;

using System;
using System.Collections.Generic;
using System.IO;
using System.Net.Security;
using System.Net.Sockets;
using System.Threading;
using System.Threading.Tasks;
using Amqp;

// AMQPNetLite 2.5.1 does not propagate connection cancellation through
// SASL/TLS and exposes no public connection Abort. Own the actual socket.
internal class AmqpSocketProvider : TransportProvider
{
    private readonly object gate = new object();
    private readonly AmqpTlsSettings tls;
    private readonly CancellationToken connectToken;
    private Socket? socket;
    private Stream? stream;
    private bool disposed;

    public AmqpSocketProvider(AmqpTlsSettings tls, CancellationToken connectToken)
    {
        this.tls = tls;
        this.connectToken = connectToken;
        this.AddressSchemes = new[] { "amqp", "amqps" };
    }

    public override async Task<IAsyncTransport> CreateAsync(Address address)
    {
        var socket = new Socket(SocketType.Stream, ProtocolType.Tcp)
        {
            NoDelay = true
        };
        lock (this.gate)
        {
            if (this.disposed)
            {
                socket.Dispose();
                throw new ObjectDisposedException(nameof(AmqpSocketProvider));
            }

            this.socket = socket;
        }

        try
        {
            await socket.ConnectAsync(address.Host, address.Port, this.connectToken).ConfigureAwait(false);
            Stream network = new NetworkStream(socket, ownsSocket: true);
            if (address.UseSsl)
            {
                network = new SslStream(network, leaveInnerStreamOpen: false);
            }

            lock (this.gate)
            {
                if (this.disposed)
                {
                    network.Dispose();
                    throw new ObjectDisposedException(nameof(AmqpSocketProvider));
                }

                this.stream = network;
            }

            if (network is SslStream secure)
            {
                await secure.AuthenticateAsClientAsync(this.tls.CreateClientOptions(address.Host), this.connectToken).ConfigureAwait(false);
            }

            this.connectToken.ThrowIfCancellationRequested();
            return new SocketTransport(this, network);
        }

        catch
        {
            this.Dispose();
            throw;
        }
    }

    public override void Dispose()
    {
        Socket? socket;
        Stream? stream;
        lock (this.gate)
        {
            if (this.disposed)
            {
                return;
            }

            this.disposed = true;
            socket = this.socket;
            stream = this.stream;
            this.socket = null;
            this.stream = null;
        }

        // Socket first; disposing TLS must not send or await close_notify.
        try
        {
            socket?.Dispose();
        }

        finally
        {
            stream?.Dispose();
        }
    }

    private class SocketTransport : IAsyncTransport
    {
        private readonly AmqpSocketProvider owner;
        private readonly Stream stream;

        public SocketTransport(AmqpSocketProvider owner, Stream stream)
        {
            this.owner = owner;
            this.stream = stream;
        }

        public void SetConnection(Connection connection)
        {
            // The library pump handles socket EOF/errors and aborts sessions.
        }

        public async Task SendAsync(IList<ByteBuffer> bufferList, int listSize)
        {
            foreach (var buffer in bufferList)
            {
                await this.stream.WriteAsync(buffer.Buffer.AsMemory(buffer.Offset, buffer.Length)).ConfigureAwait(false);
            }
        }

        public Task<int> ReceiveAsync(byte[] buffer, int offset, int count)
        {
            return this.stream.ReadAsync(buffer.AsMemory(offset, count)).AsTask();
        }

        public void Close()
        {
            this.owner.Dispose();
        }

        public void Send(ByteBuffer buffer)
        {
            throw new NotSupportedException("Only asynchronous AMQP transport operations are supported.");
        }

        public int Receive(byte[] buffer, int offset, int count)
        {
            throw new NotSupportedException("Only asynchronous AMQP transport operations are supported.");
        }
    }
}