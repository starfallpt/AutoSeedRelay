<template>
  <el-container class="layout">
    <el-aside width="220px" class="aside">
      <div class="brand">AutoSeedRelay</div>
      <el-menu
        :default-active="activeMenu"
        router
        class="menu"
        background-color="#1f2d3d"
        text-color="#c0c4cc"
        active-text-color="#409eff"
      >
        <el-menu-item v-for="item in menu" :key="item.path" :index="item.path">
          <span>{{ item.title }}</span>
        </el-menu-item>
      </el-menu>
    </el-aside>

    <el-container>
      <el-header class="header">
        <div class="page-title">{{ pageTitle }}</div>
        <el-button text @click="onLogout">退出登录</el-button>
      </el-header>

      <el-main class="main">
        <router-view />
      </el-main>
    </el-container>
  </el-container>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const menu = [
  { path: '/dashboard', title: '仪表盘' },
  { path: '/seeds', title: '种子' },
  { path: '/events', title: '事件' },
  { path: '/sources', title: '站点源' },
  { path: '/targets', title: '目标站' },
  { path: '/qb', title: 'qB 实例' },
  { path: '/strategy', title: '策略' },
  { path: '/notifiers', title: '通知' },
  { path: '/backup', title: '备份' },
]

const activeMenu = computed(() => route.path)
const pageTitle = computed(() => (route.meta.title as string) ?? '')

async function onLogout() {
  await auth.logout()
  ElMessage.success('已退出登录')
  router.push('/login')
}
</script>

<style scoped>
.layout {
  height: 100vh;
}

.aside {
  background: #1f2d3d;
  display: flex;
  flex-direction: column;
}

.brand {
  height: 60px;
  line-height: 60px;
  text-align: center;
  color: #fff;
  font-size: 18px;
  font-weight: 600;
  border-bottom: 1px solid rgba(255, 255, 255, 0.1);
}

.menu {
  flex: 1;
  border-right: none;
}

.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  background: #fff;
  border-bottom: 1px solid #e4e7ed;
}

.page-title {
  font-size: 18px;
  font-weight: 600;
  color: #303133;
}

.main {
  background: #f5f7fa;
  padding: 16px;
}
</style>
