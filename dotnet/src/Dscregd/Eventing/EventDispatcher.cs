namespace Dscregd.Eventing;

public class EventDispatcher
{
    private readonly EventWorker? worker;

    public EventDispatcher()
    {
    }

    public EventDispatcher(EventWorker worker)
    {
        this.worker = worker;
    }

    public void Notify(string type, string machineId, string? configurationName = null, string? reportId = null)
    {
        this.worker?.Notify(type, machineId, configurationName, reportId);
    }
}