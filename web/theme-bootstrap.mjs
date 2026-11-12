// 内联在构建后的 HTML 中，使主题在样式绘制前生效。
export function themeBootstrap(consolePage) {
  const route = consolePage ? "if(location.pathname==='/login'||location.pathname==='/change-password')d.classList.add('route-login')" : '';
  const navigation = consolePage ? "try{if(localStorage.getItem('orbitbeacon.nav')==='collapsed')d.classList.add('nav-collapsed')}catch(e){}if(matchMedia('(min-width: 768px) and (max-width: 1199px)').matches)d.classList.add('nav-compact');" : '';
  return `<script>(function(d){try{var t=localStorage.getItem('orbitbeacon-theme');if(t==='light'||t==='dark')d.dataset.theme=t}catch(e){}${navigation}${route}})(document.documentElement)</script>`;
}
