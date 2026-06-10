namespace Fixture.Core.Storage; //# def namespace Fixture.Core.Storage

public delegate void ChangedHandler(string key); //# def delegate Fixture.Core.Storage.ChangedHandler

public interface IRepository<T> where T : notnull //# def interface Fixture.Core.Storage.IRepository
{
    T? Find(int id); //# def method Fixture.Core.Storage.IRepository.Find
    void Save(T item); //# def method Fixture.Core.Storage.IRepository.Save
}

public abstract class RepositoryBase<T> : IRepository<T> where T : notnull //# def class Fixture.Core.Storage.RepositoryBase; impl RepositoryBase of IRepository; ref IRepository
{
    public event ChangedHandler? Changed; //# ref ChangedHandler

    public abstract T? Find(int id); //# def method Fixture.Core.Storage.RepositoryBase.Find

    public abstract void Save(T item); //# def method Fixture.Core.Storage.RepositoryBase.Save

    protected void NotifyChanged(string key) //# def method Fixture.Core.Storage.RepositoryBase.NotifyChanged
    {
        Changed?.Invoke(key);
    }
}
