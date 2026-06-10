using System.Collections.Generic;
using System.Threading.Tasks;

namespace Fixture.Core.Storage; //# def namespace Fixture.Core.Storage

public record SyncReport //# def record Fixture.Core.Storage.SyncReport
{
    public SyncReport(int total) //# def constructor Fixture.Core.Storage.SyncReport.SyncReport
    {
        Total = total; //# ref Total
    }

    public int Total { get; } //# def property Fixture.Core.Storage.SyncReport.Total
}

public class InMemoryRepository<T> : RepositoryBase<T> where T : notnull //# def class Fixture.Core.Storage.InMemoryRepository; impl InMemoryRepository of RepositoryBase; ref RepositoryBase
{
    private readonly Dictionary<int, T> items = new();
    private int nextId;

    public InMemoryRepository(int capacity) //# def constructor Fixture.Core.Storage.InMemoryRepository.InMemoryRepository; implicit RepositoryBase
    {
        nextId = capacity > 0 ? 1 : 0;
    }

    public int Count => items.Count; //# def property Fixture.Core.Storage.InMemoryRepository.Count

    public override T? Find(int id) //# def method Fixture.Core.Storage.InMemoryRepository.Find
    {
        return items.TryGetValue(id, out T? item) ? item : default;
    }

    public override void Save(T item) //# def method Fixture.Core.Storage.InMemoryRepository.Save
    {
        items[nextId] = item;
        NotifyChanged(nextId.ToString()); //# ref NotifyChanged; call InMemoryRepository.Save -> NotifyChanged
        nextId++;
    }

    public async Task<int> CountAsync() //# def method Fixture.Core.Storage.InMemoryRepository.CountAsync
    {
        await Task.Yield();
        return Count; //# ref Count
    }

    public async Task<SyncReport> SyncAsync() //# def method Fixture.Core.Storage.InMemoryRepository.SyncAsync; ref SyncReport
    {
        await Task.Yield();
        return new SyncReport(Count); //# ref SyncReport; ref Count; call SyncAsync -> SyncReport.SyncReport
    }
}
