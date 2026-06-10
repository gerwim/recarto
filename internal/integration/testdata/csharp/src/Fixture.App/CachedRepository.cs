using System.Collections.Generic;

namespace Fixture.App; //# def namespace Fixture.App

[System.Obsolete("prefer the in-memory repository")] //# annot Obsolete on CachedRepository
public class CachedRepository : Fixture.Core.Storage.IRepository<string> //# def class Fixture.App.CachedRepository; impl CachedRepository of IRepository; ref IRepository
{
    private readonly Dictionary<int, string> cache = new();
    private int nextSlot;

    public string? Find(int id) //# def method Fixture.App.CachedRepository.Find
    {
        return cache.TryGetValue(id, out string? hit) ? hit : null;
    }

    public void Save(string item) //# def method Fixture.App.CachedRepository.Save
    {
        cache[nextSlot] = item;
        nextSlot++;
    }
}
