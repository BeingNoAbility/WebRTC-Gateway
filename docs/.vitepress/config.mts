import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

export default withMermaid(
  defineConfig({
    base: '/WebRTC-Gateway/',
    cleanUrls: true,
    lastUpdated: true,

    // Exclude single-version release notes from VitePress routes to protect release.yml pipeline
    srcExclude: ['**/RELEASE_NOTES.md'],

    head: [
      ['link', { rel: 'icon', type: 'image/svg+xml', href: '/WebRTC-Gateway/logo.svg' }],
      ['meta', { name: 'theme-color', content: '#38bdf8' }],
      ['meta', { property: 'og:type', content: 'website' }],
      ['meta', { property: 'og:title', content: 'WebRTC Gateway - 浏览器零客户端 P2P 内网穿透网关' }],
      ['meta', { property: 'og:description', content: '无需安装客户端或插件，直接在现代浏览器中秒级直连内网 Web 服务、NAS 与私有云。' }],
    ],

    locales: {
      root: {
        label: '简体中文',
        lang: 'zh-CN',
        title: 'WebRTC Gateway',
        description: '浏览器零客户端 P2P 内网穿透网关',
        themeConfig: {
          logo: '/logo.svg',
          siteTitle: 'WebRTC Gateway',

          nav: [
            { text: '首页', link: '/' },
            { text: '指南概览', link: '/guide/' },
            { text: '架构与协议', link: '/guide/architecture' },
            { text: '生产部署', link: '/guide/deployment' },
            { text: '发版门禁清单', link: '/RELEASE_CHECKLIST' },
            { text: 'GitHub 仓库', link: 'https://github.com/BeingNoAbility/WebRTC-Gateway' },
          ],

          sidebar: [
            {
              text: '入门指南 (Getting Started)',
              collapsed: false,
              items: [
                { text: '文档概览', link: '/guide/' },
                { text: '架构与设计原理', link: '/guide/architecture' },
                { text: 'P2PMux 协议形式化规约', link: '/guide/protocol-specification' },
                { text: '浏览器兼容性矩阵', link: '/guide/browser-compatibility' },
              ],
            },
            {
              text: '部署与配置 (Deployment & Config)',
              collapsed: false,
              items: [
                { text: '生产部署指南', link: '/guide/deployment' },
                { text: 'Agent 配置完全手册', link: '/guide/agent-configuration' },
                { text: 'P2P 文件与目录服务', link: '/guide/file-server' },
                { text: '安全与权限控制', link: '/guide/security' },
                { text: '故障排查与 FAQ', link: '/guide/troubleshooting' },
              ],
            },
            {
              text: '工程规范与质检 (Specifications)',
              collapsed: false,
              items: [
                { text: '正式发版质量审计清单', link: '/RELEASE_CHECKLIST' },
                { text: '辩证审查原则与核对清单', link: '/REVIEW_GUIDELINES' },
                { text: '全息实战与真实环境测试清单', link: '/TESTING_CHECKLIST' },
                { text: '文档与文案工程规范', link: '/STYLE_GUIDE' },
              ],
            },
          ],

          docFooter: {
            prev: '上一篇',
            next: '下一篇',
          },

          outline: {
            level: [2, 3],
            label: '本页目录',
          },

          lastUpdated: {
            text: '最后更新于',
          },
        },
      },

      en: {
        label: 'English',
        lang: 'en-US',
        link: '/en/',
        title: 'WebRTC Gateway',
        description: 'Zero-Client P2P WebRTC Intranet Penetration Gateway',
        themeConfig: {
          logo: '/logo.svg',
          siteTitle: 'WebRTC Gateway',

          nav: [
            { text: 'Home', link: '/en/' },
            { text: 'Overview', link: '/en/guide/' },
            { text: 'Architecture', link: '/en/guide/architecture' },
            { text: 'Deployment', link: '/en/guide/deployment' },
            { text: 'Checklist', link: '/en/RELEASE_CHECKLIST' },
            { text: 'GitHub', link: 'https://github.com/BeingNoAbility/WebRTC-Gateway' },
          ],

          sidebar: [
            {
              text: 'Getting Started',
              collapsed: false,
              items: [
                { text: 'Overview & Quickstart', link: '/en/guide/' },
                { text: 'Architecture & Design', link: '/en/guide/architecture' },
                { text: 'P2PMux Wire Protocol Specification', link: '/guide/protocol-specification' },
                { text: 'Browser Compatibility', link: '/en/guide/browser-compatibility' },
              ],
            },
            {
              text: 'Deployment & Configuration',
              collapsed: false,
              items: [
                { text: 'Production Deployment', link: '/en/guide/deployment' },
                { text: 'Agent & Server Configuration', link: '/en/guide/agent-configuration' },
                { text: 'P2P File & Directory Server', link: '/en/guide/file-server' },
                { text: 'Security & Access Control', link: '/en/guide/security' },
                { text: 'Troubleshooting & FAQ', link: '/en/guide/troubleshooting' },
              ],
            },
            {
              text: 'Engineering Specifications',
              collapsed: false,
              items: [
                { text: 'Release Quality Checklist', link: '/en/RELEASE_CHECKLIST' },
                { text: 'Dialectical Review Guidelines', link: '/en/REVIEW_GUIDELINES' },
                { text: 'Real-World & Browser Testing Checklist', link: '/en/TESTING_CHECKLIST' },
                { text: 'Documentation Style Guide', link: '/en/STYLE_GUIDE' },
              ],
            },
          ],

          docFooter: {
            prev: 'Previous Page',
            next: 'Next Page',
          },

          outline: {
            level: [2, 3],
            label: 'On this page',
          },

          lastUpdated: {
            text: 'Last updated',
          },
        },
      },
    },

    themeConfig: {
      search: {
        provider: 'local',
        options: {
          locales: {
            root: {
              translations: {
                button: {
                  buttonText: '搜索文档',
                  buttonAriaLabel: '搜索文档',
                },
                modal: {
                  noResultsText: '无法找到相关结果',
                  resetButtonTitle: '清除查询条件',
                  footer: {
                    selectText: '选择',
                    navigateText: '切换',
                    closeText: '关闭',
                  },
                },
              },
            },
            en: {
              translations: {
                button: {
                  buttonText: 'Search Docs',
                  buttonAriaLabel: 'Search Docs',
                },
                modal: {
                  noResultsText: 'No results found',
                  resetButtonTitle: 'Clear query',
                  footer: {
                    selectText: 'Select',
                    navigateText: 'Navigate',
                    closeText: 'Close',
                  },
                },
              },
            },
          },
        },
      },

      socialLinks: [
        { icon: 'github', link: 'https://github.com/BeingNoAbility/WebRTC-Gateway' },
      ],

      footer: {
        message: 'Released under the MIT License.',
        copyright: 'Copyright © 2026 BeingNoAbility. All rights reserved.',
      },
    },

    mermaid: {},
  })
)
