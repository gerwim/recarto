use std::collections::HashMap;

pub trait Repository<T> { //# def trait Repository
    fn save(&mut self, item: T) -> usize; //# def method Repository.save
    fn find(&self, id: usize) -> Option<&T>; //# def method Repository.find
}

pub struct InMemoryRepository<T> { //# def struct InMemoryRepository
    items: HashMap<usize, T>,
    next_id: usize,
}

impl<T> InMemoryRepository<T> { //# def impl InMemoryRepository
    pub fn with_capacity(capacity: usize) -> Self { //# def method InMemoryRepository.with_capacity; implicit InMemoryRepository
        InMemoryRepository { //# ref InMemoryRepository
            items: HashMap::with_capacity(capacity), //# tok with_capacity
            next_id: 1,
        }
    }

    pub fn count(&self) -> usize { //# def method InMemoryRepository.count
        self.items.len()
    }
}

impl<T> Repository<T> for InMemoryRepository<T> { //# def impl InMemoryRepository; impl InMemoryRepository of Repository; ref Repository
    fn save(&mut self, item: T) -> usize { //# def method InMemoryRepository.save
        let id = self.next_id;
        self.items.insert(id, item);
        self.next_id += 1;
        id
    }

    fn find(&self, id: usize) -> Option<&T> { //# def method InMemoryRepository.find
        self.items.get(&id)
    }
}
