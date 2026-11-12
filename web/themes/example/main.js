import './public.css';
import {mountPublicPage} from '../../src/public/page.js';
import {createCard, skeletonCards} from '../default/cards.js';
import {createConstellation} from '../default/constellation.js';

mountPublicPage({createCard, skeletonCards, createConstellation});
