// IndexedDB 封装（网页模式用来把对象密钥交给 Service Worker）。
// 注意：public/sw.js 中有一份相同结构的实现，修改时两边保持一致。
const DB_NAME = 'veilfin' // sw.js 里有同样的值
const VERSION = 1

let dbp = null

export function openDB() {
  if (!dbp) {
    dbp = new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, VERSION)
      req.onupgradeneeded = () => {
        const db = req.result
        if (!db.objectStoreNames.contains('blobs')) db.createObjectStore('blobs')
        if (!db.objectStoreNames.contains('meta')) db.createObjectStore('meta')
      }
      req.onsuccess = () => {
        const db = req.result
        db.onclose = () => (dbp = null)
        db.onversionchange = () => {
          db.close()
          dbp = null
        }
        resolve(db)
      }
      req.onerror = () => {
        dbp = null
        reject(req.error)
      }
    })
  }
  return dbp
}

async function withDB(fn) {
  for (let attempt = 0; attempt < 2; attempt++) {
    const db = await openDB()
    try {
      return await fn(db)
    } catch (e) {
      dbp = null
      if (attempt === 1) throw e
    }
  }
}

function done(tx) {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve()
    tx.onerror = () => reject(tx.error)
    tx.onabort = () => reject(tx.error)
  })
}

export function putMany(store, entries) {
  return withDB((db) => {
    const tx = db.transaction(store, 'readwrite')
    const s = tx.objectStore(store)
    for (const [k, v] of entries) s.put(v, k)
    return done(tx)
  })
}

export function put(store, key, value) {
  return putMany(store, [[key, value]])
}

export function get(store, key) {
  return withDB(
    (db) =>
      new Promise((resolve, reject) => {
        const req = db.transaction(store).objectStore(store).get(key)
        req.onsuccess = () => resolve(req.result)
        req.onerror = () => reject(req.error)
      }),
  )
}

export function clear(store) {
  return withDB((db) => {
    const tx = db.transaction(store, 'readwrite')
    tx.objectStore(store).clear()
    return done(tx)
  })
}
