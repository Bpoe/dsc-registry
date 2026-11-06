namespace Dscregd.Eventing;

using System;
using System.Threading;
using System.Threading.Tasks;

// The worker/transport boundary permits deterministic queue and shutdown tests.
public interface IEventPublisher : IDisposable
{
    // Completes successfully only for an accepted delivery; honors cancellation.
    public Task PublishAsync(RegistryEvent notification, CancellationToken cancellationToken);

    // Interrupts outstanding I/O immediately, without a protocol close handshake.
    public void Abort();
}