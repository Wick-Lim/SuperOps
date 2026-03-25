import React from 'react'
import { View, Text, SafeAreaView } from 'react-native'

export default function AdminScreen() {
  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center' }}>
        <Text style={{ color: '#fff', fontSize: 18, fontWeight: '600' }}>Admin Panel</Text>
        <Text style={{ color: '#94a3b8', marginTop: 8 }}>Coming soon</Text>
      </View>
    </SafeAreaView>
  )
}
