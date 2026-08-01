export class AccountCache<K, V> {
  private values = new Map<K, V>()
  private epoch = 0

  get generation(): number {
    return this.epoch
  }

  get(key: K): V | undefined {
    return this.values.get(key)
  }

  has(key: K): boolean {
    return this.values.has(key)
  }

  set(key: K, value: V): void {
    this.values.set(key, value)
  }

  setIfCurrent(generation: number, key: K, value: V): boolean {
    if (generation !== this.epoch) return false
    this.values.set(key, value)
    return true
  }

  clear(): void {
    this.epoch += 1
    this.values.clear()
  }
}
